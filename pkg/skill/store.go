package skill

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// SkillStore is the consumer-facing aggregate over backends.
// Implementations should not panic on a single backend failure;
// List joins partial errors and still surfaces the valid skills
// it could read.
type SkillStore interface {
	// List returns every skill across every backend. The returned
	// error joins per-backend failures via errors.Join — callers
	// can errors.Is against ErrManifestInvalid to count parse
	// failures separately.
	List(ctx context.Context) ([]Skill, error)

	// Get fetches a single skill by name. Search order is system
	// → local → community → inline → hub; first hit wins. Returns
	// ErrSkillNotFound when no backend has the name.
	Get(ctx context.Context, name string) (Skill, error)

	// Publish writes a skill to the local:// backend. Returns
	// ErrUnsupportedBackend if the store has no writable backend.
	Publish(ctx context.Context, m Manifest, body fs.FS) error
}

// Backend is one origin of skills (system / community / local /
// inline / hub). The store iterates backends in priority order
// per Get's contract.
//
// Push-source backends (e.g. a future hub backend that subscribes
// to remote "skill_published" events) hold a *Store reference at
// construction and call Store.Refresh from their event handler —
// invalidation flows through the same seam Publish uses, so the
// cache layer stays unaware of where the change came from.
type Backend interface {
	Origin() Origin
	List(ctx context.Context) ([]Skill, error)
	Get(ctx context.Context, name string) (Skill, error)
	// Publish optional; backends that don't support write return
	// ErrUnsupportedBackend.
	Publish(ctx context.Context, m Manifest, body fs.FS) error
}

// Options groups the directory/inline configuration that
// NewSkillStore consumes. Fields are independent — leave any
// of them empty to skip that backend.
type Options struct {
	SystemRoot    string                // ${HUGEN_STATE}/skills/system/
	CommunityRoot string                // deployment-pinned read-only root
	LocalRoot     string                // ${HUGEN_STATE}/skills/local/ (writable)
	Inline        map[string][]byte     // map[name]frontmatter+body
}

// NewSkillStore wires up backends from Options. Always returns a
// non-nil *Store; missing roots simply contribute zero skills to
// List/Get.
func NewSkillStore(opts Options) *Store {
	s := &Store{}
	if opts.SystemRoot != "" {
		s.backends = append(s.backends, &dirBackend{origin: OriginSystem, root: opts.SystemRoot, writable: false})
	}
	if opts.LocalRoot != "" {
		s.backends = append(s.backends, &dirBackend{origin: OriginLocal, root: opts.LocalRoot, writable: true})
	}
	if opts.CommunityRoot != "" {
		s.backends = append(s.backends, &dirBackend{origin: OriginCommunity, root: opts.CommunityRoot, writable: false})
	}
	if len(opts.Inline) > 0 {
		s.backends = append(s.backends, newInlineBackend(opts.Inline))
	}
	// hub:// stub — always last; always rejects List/Get/Publish
	// with ErrUnsupportedBackend.
	s.backends = append(s.backends, &hubBackend{})
	return s
}

// Store satisfies SkillStore by fanning across registered Backend
// implementations. Results from List are cached; the cache is
// invalidated by Publish (writable backends) and by an explicit
// Refresh call (covers operator-side mutations like dropping a
// new skill into ${HUGEN_STATE}/skills/local/ without going
// through Publish).
type Store struct {
	backends []Backend

	cacheMu  sync.RWMutex
	cacheGen int64
	cached   []Skill
	cachedAt int64 // gen value at which cached was populated
	cacheErr error
	gen      int64 // bumped on every Publish / Refresh
}

// Compile-time assertion.
var _ SkillStore = (*Store)(nil)

func (s *Store) List(ctx context.Context) ([]Skill, error) {
	s.cacheMu.RLock()
	if s.cached != nil && s.cachedAt == s.currentGen() {
		out := append([]Skill(nil), s.cached...)
		err := s.cacheErr
		s.cacheMu.RUnlock()
		return out, err
	}
	s.cacheMu.RUnlock()
	return s.refreshList(ctx)
}

// Refresh invalidates the List cache so the next call re-scans
// every backend. Use after an out-of-band mutation (operator
// dropped a new skill directory into the local/ root, an admin
// pulled in a community update, etc.) — Publish does this
// automatically and does not need a follow-up Refresh.
func (s *Store) Refresh() {
	s.cacheMu.Lock()
	s.gen++
	s.cached = nil
	s.cacheErr = nil
	s.cacheMu.Unlock()
}

func (s *Store) currentGen() int64 {
	s.cacheMu.RLock()
	defer s.cacheMu.RUnlock()
	return s.gen
}

func (s *Store) refreshList(ctx context.Context) ([]Skill, error) {
	var out []Skill
	var errs []error
	for _, b := range s.backends {
		if _, isHub := b.(*hubBackend); isHub {
			continue
		}
		got, err := b.List(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", b.Origin(), err))
		}
		out = append(out, got...)
	}
	joined := errors.Join(errs...)
	s.cacheMu.Lock()
	s.cached = out
	s.cachedAt = s.gen
	s.cacheErr = joined
	s.cacheMu.Unlock()
	return append([]Skill(nil), out...), joined
}

func (s *Store) Get(ctx context.Context, name string) (Skill, error) {
	for _, b := range s.backends {
		got, err := b.Get(ctx, name)
		if err == nil {
			return got, nil
		}
		if errors.Is(err, ErrSkillNotFound) {
			continue
		}
		// Real error — surface it; the caller will know which
		// backend failed.
		return Skill{}, fmt.Errorf("%s: %w", b.Origin(), err)
	}
	return Skill{}, ErrSkillNotFound
}

func (s *Store) Publish(ctx context.Context, m Manifest, body fs.FS) error {
	for _, b := range s.backends {
		err := b.Publish(ctx, m, body)
		if err == nil {
			s.Refresh()
			return nil
		}
		if errors.Is(err, ErrUnsupportedBackend) {
			continue
		}
		return fmt.Errorf("%s: %w", b.Origin(), err)
	}
	return ErrUnsupportedBackend
}

// --- directory-backed backend (system / community / local) ---

type dirBackend struct {
	origin   Origin
	root     string
	writable bool
}

func (b *dirBackend) Origin() Origin { return b.origin }

func (b *dirBackend) List(ctx context.Context) ([]Skill, error) {
	if b.root == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(b.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read dir %s: %w", b.root, err)
	}
	var out []Skill
	var errs []error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if err := ctx.Err(); err != nil {
			return out, err
		}
		skill, err := b.readSkillDir(filepath.Join(b.root, e.Name()))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		out = append(out, skill)
	}
	return out, errors.Join(errs...)
}

func (b *dirBackend) Get(ctx context.Context, name string) (Skill, error) {
	if b.root == "" {
		return Skill{}, ErrSkillNotFound
	}
	if err := ctx.Err(); err != nil {
		return Skill{}, err
	}
	dir := filepath.Join(b.root, name)
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Skill{}, ErrSkillNotFound
		}
		return Skill{}, fmt.Errorf("stat %s: %w", dir, err)
	}
	return b.readSkillDir(dir)
}

func (b *dirBackend) readSkillDir(dir string) (Skill, error) {
	manifestPath := filepath.Join(dir, "SKILL.md")
	content, err := os.ReadFile(manifestPath)
	if err != nil {
		return Skill{}, fmt.Errorf("read %s: %w", manifestPath, err)
	}
	m, err := Parse(content)
	if err != nil {
		return Skill{}, err
	}
	return Skill{
		Manifest: m,
		Origin:   b.origin,
		FS:       os.DirFS(dir),
	}, nil
}

func (b *dirBackend) Publish(ctx context.Context, m Manifest, body fs.FS) error {
	if !b.writable {
		return ErrUnsupportedBackend
	}
	if b.root == "" {
		return ErrUnsupportedBackend
	}
	dir := filepath.Join(b.root, m.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	// SKILL.md = original frontmatter (m.Raw) + body (m.Body) when
	// present; otherwise re-emit minimal frontmatter from the
	// validated manifest.
	manifestPath := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(manifestPath, encodeManifest(m), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", manifestPath, err)
	}
	if body != nil {
		if err := copyFS(dir, body); err != nil {
			return fmt.Errorf("copy body: %w", err)
		}
	}
	return nil
}

// encodeManifest builds the SKILL.md content for Publish. Prefers
// the original Raw bytes when available so a publish-then-fetch
// round-trip preserves comments and key order; otherwise falls
// back to a minimal "name + description + license" preamble.
func encodeManifest(m Manifest) []byte {
	const sep = "---\n"
	if len(m.Raw) > 0 {
		out := []byte(sep)
		out = append(out, m.Raw...)
		out = append(out, '\n')
		out = append(out, []byte(sep)...)
		out = append(out, m.Body...)
		return out
	}
	min := fmt.Sprintf("%sname: %s\ndescription: %s\nlicense: %s\n%s", sep, m.Name, m.Description, m.License, sep)
	return append([]byte(min), m.Body...)
}

func copyFS(dst string, src fs.FS) error {
	return fs.WalkDir(src, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == "." {
			return nil
		}
		// Skip SKILL.md — already written by Publish.
		if path == "SKILL.md" {
			return nil
		}
		target := filepath.Join(dst, path)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(src, path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// --- inline backend (test fixtures) ---

type inlineBackend struct {
	mu      sync.RWMutex
	entries map[string]Skill
}

func newInlineBackend(raw map[string][]byte) *inlineBackend {
	b := &inlineBackend{entries: make(map[string]Skill, len(raw))}
	for name, data := range raw {
		m, err := Parse(data)
		if err != nil {
			// Inline skills that fail to parse are dropped at
			// construction time; List/Get just won't see them.
			// Logging is the caller's concern.
			continue
		}
		if m.Name == "" {
			m.Name = name
		}
		b.entries[m.Name] = Skill{Manifest: m, Origin: OriginInline}
	}
	return b
}

func (b *inlineBackend) Origin() Origin { return OriginInline }

func (b *inlineBackend) List(ctx context.Context) ([]Skill, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]Skill, 0, len(b.entries))
	for _, s := range b.entries {
		out = append(out, s)
	}
	return out, nil
}

func (b *inlineBackend) Get(ctx context.Context, name string) (Skill, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	s, ok := b.entries[name]
	if !ok {
		return Skill{}, ErrSkillNotFound
	}
	return s, nil
}

func (b *inlineBackend) Publish(ctx context.Context, m Manifest, body fs.FS) error {
	return ErrUnsupportedBackend
}

// --- hub stub (phase 7 implements) ---

type hubBackend struct{}

func (b *hubBackend) Origin() Origin                                   { return OriginHub }
func (b *hubBackend) List(ctx context.Context) ([]Skill, error)        { return nil, ErrUnsupportedBackend }
func (b *hubBackend) Get(ctx context.Context, name string) (Skill, error) {
	return Skill{}, ErrSkillNotFound
}
func (b *hubBackend) Publish(ctx context.Context, m Manifest, body fs.FS) error {
	return ErrUnsupportedBackend
}
