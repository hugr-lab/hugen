package skill

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/hugr-lab/query-engine/types"
)

// CleanRelPath validates a user-supplied relative path for use as
// a key inside a skill bundle (references/scripts/assets entries
// in skill:save). Returns the cleaned slash-separated path or
// wraps ErrInvalidPath. Rejects:
//
//   - empty paths;
//   - absolute paths (leading "/");
//   - paths containing ".." segments (escapes bundle root);
//   - paths containing NUL bytes or backslashes (cross-OS safety);
//   - hidden paths (segments starting with ".") — keeps the
//     bundle layout transparent;
//   - non-normalised paths (path.Clean(p) != p), so the bundle's
//     on-disk shape matches what the manifest body references.
//
// The bundle's category prefix (e.g. "scripts/") is the caller's
// responsibility — CleanRelPath operates on the path WITHIN the
// category map.
func CleanRelPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("%w: empty path", ErrInvalidPath)
	}
	if strings.ContainsAny(p, "\x00\\") {
		return "", fmt.Errorf("%w: %q contains NUL or backslash", ErrInvalidPath, p)
	}
	if strings.HasPrefix(p, "/") || path.IsAbs(p) {
		return "", fmt.Errorf("%w: %q is absolute", ErrInvalidPath, p)
	}
	cleaned := path.Clean(p)
	if cleaned != p {
		return "", fmt.Errorf("%w: %q is not normalised (cleaned to %q)", ErrInvalidPath, p, cleaned)
	}
	for _, seg := range strings.Split(cleaned, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("%w: %q contains forbidden segment %q", ErrInvalidPath, p, seg)
		}
		if strings.HasPrefix(seg, ".") {
			return "", fmt.Errorf("%w: %q has hidden segment %q", ErrInvalidPath, p, seg)
		}
	}
	return cleaned, nil
}

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
	// ErrUnsupportedBackend if the store has no writable backend;
	// ErrSkillExists if a skill with this name already exists and
	// opts.Overwrite is false.
	Publish(ctx context.Context, m Manifest, body fs.FS, opts PublishOptions) error
}

// PublishOptions controls how SkillStore.Publish handles edge
// cases. Zero value = safe defaults (no overwrite).
type PublishOptions struct {
	// Overwrite, when true, replaces an existing bundle of the
	// same name (the existing directory is removed first to avoid
	// stale leftovers). Default false — collision returns
	// ErrSkillExists. The save protocol asks the user explicitly
	// before retrying with Overwrite=true; the post-save
	// validation iteration loop sets it without prompting.
	Overwrite bool
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
	// ErrUnsupportedBackend. Honour opts.Overwrite per the
	// SkillStore.Publish contract.
	Publish(ctx context.Context, m Manifest, body fs.FS, opts PublishOptions) error
}

// Options groups the configuration NewSkillStore consumes. Fields
// are independent — leave any of them empty to skip that backend.
type Options struct {
	// SystemFS is the embed.FS that holds the agent-core
	// skills (`_root`, `_mission`, …). Read-only, no on-disk
	// presence. nil disables the system backend (tests).
	SystemFS fs.FS
	// HubRoot is the on-disk path where the deployment's hub
	// skills are installed (today filled from the binary's
	// embedded bundle; in the future, synced from a remote Hugr
	// hub function). Read-only — operator edits are clobbered
	// at install time. Empty disables the hub backend.
	HubRoot string
	// LocalRoot is `${state}/skills/local/`, writable via
	// skill:save. Empty disables the local backend.
	//
	// Phase 6.2.db: when DynamicQuerier is non-nil, LocalRoot
	// becomes the on-disk bundle root for the DB-indexed dynamic
	// backend (consolidating the plain local dirBackend); without a
	// querier it stays the plain writable dirBackend (tests / no
	// engine).
	LocalRoot string
	// DynamicQuerier, when non-nil, upgrades the LocalRoot backend
	// to the Phase-6.2.db dynamic backend (dir + DB index). Requires
	// AgentID. Nil keeps the plain local dirBackend.
	DynamicQuerier types.Querier
	// AgentID is the multi-tenant scope key for the dynamic index.
	// Required when DynamicQuerier is set.
	AgentID string
	// EmbedderEnabled gates semantic indexing on the dynamic
	// backend: when true, publish/reconcile pass the description as
	// `summary:` so Hugr regenerates description_vec server-side.
	EmbedderEnabled bool
	// Logger is used by the dynamic backend to emit Debug lines for
	// install / pin / catalog-link actions. Nil → a discard logger.
	Logger *slog.Logger
	// Inline is the in-memory channel used by tests and the
	// skill:save tool while a session keeps a freshly-authored
	// skill before flushing to local.
	Inline map[string][]byte
}

// NewSkillStore wires up backends from Options. Always returns a
// non-nil *Store; empty / nil fields simply contribute zero
// skills to List / Get.
func NewSkillStore(opts Options) *Store {
	s := &Store{}
	if opts.SystemFS != nil {
		s.backends = append(s.backends, &embedBackend{origin: OriginSystem, fs: opts.SystemFS})
	}
	dynamicWired := opts.DynamicQuerier != nil && opts.AgentID != ""
	// Hub backend: only a standalone read-only dirBackend when there's
	// NO dynamic backend (tests / no-engine). When dynamic is wired the
	// hub bundles are INDEXED into `skills` (Store.SyncDynamic, bundle_
	// path → the hub dir) so they surface through the dynamic backend —
	// a separate hub backend would double-list them.
	if opts.HubRoot != "" && !dynamicWired {
		s.backends = append(s.backends, &dirBackend{origin: OriginHub, root: opts.HubRoot, writable: false})
	}
	if opts.LocalRoot != "" {
		if dynamicWired {
			s.dynamic = newDynamicBackend(opts.LocalRoot, opts.DynamicQuerier, opts.AgentID, opts.EmbedderEnabled, opts.Logger)
			s.backends = append(s.backends, s.dynamic)
		} else {
			s.backends = append(s.backends, &dirBackend{origin: OriginLocal, root: opts.LocalRoot, writable: true})
		}
	}
	if len(opts.Inline) > 0 {
		s.backends = append(s.backends, newInlineBackend(opts.Inline))
	}
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

	// dynamic is the Phase-6.2.db backend when one was wired (nil
	// otherwise). Held separately from backends so the runtime can
	// reach Reconcile / Uninstall without a type assertion over the
	// backend slice.
	dynamic *dynamicBackend

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

func (s *Store) Publish(ctx context.Context, m Manifest, body fs.FS, opts PublishOptions) error {
	for _, b := range s.backends {
		err := b.Publish(ctx, m, body, opts)
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

// HasDynamic reports whether a Phase-6.2.db dynamic backend is wired.
func (s *Store) HasDynamic() bool { return s.dynamic != nil }

// Search runs semantic discovery over the dynamic index — the PRIMARY
// discovery path when an embedder is wired. Returns ErrNoEmbedder when
// no dynamic backend is present OR the backend has no embedder, so the
// caller can fall back to its keyword/substring path (the notepad
// precedent). Results are semantically ranked + capped at opts.Limit.
func (s *Store) Search(ctx context.Context, query string, opts SearchOpts) ([]Skill, error) {
	if s.dynamic == nil {
		return nil, ErrNoEmbedder
	}
	rows, err := s.dynamic.index.search(ctx, query, opts.TaskEligible, opts.Type, opts.Limit)
	if err != nil {
		return nil, err
	}
	out := make([]Skill, 0, len(rows))
	for _, r := range rows {
		sk, rerr := rowToSkill(r)
		if rerr != nil {
			continue
		}
		out = append(out, sk)
	}
	return out, nil
}

// Reconcile re-indexes the dynamic backend's writable (authored)
// bundles into the DB + relinks catalogs (no-op when no dynamic
// backend is wired). Invalidates the List cache so the next read
// reflects the reconciled index.
func (s *Store) Reconcile(ctx context.Context) (int, error) {
	if s.dynamic == nil {
		return 0, nil
	}
	n, err := s.dynamic.Reconcile(ctx)
	s.Refresh()
	return n, err
}

// SyncDynamic is the boot/refresh entry: install the hub bundles from
// `hubDir` into the index per the install set, then reconcile the
// writable (authored) dir + relink catalogs across the whole index.
// Per-source dirs stay on disk (hub / local); only the index is
// unified. No-op when no dynamic backend is wired. Returns the total
// bundles indexed.
func (s *Store) SyncDynamic(ctx context.Context, hubDir string, installSet []string, declared bool) (int, error) {
	if s.dynamic == nil {
		return 0, nil
	}
	installed, ierr := s.dynamic.installFromDir(ctx, hubDir, "hub", installSet, declared)
	indexed, rerr := s.dynamic.Reconcile(ctx) // authored + relink (sees hub catalogs)
	s.Refresh()
	return installed + indexed, errors.Join(ierr, rerr)
}

// CatalogMembers returns the member skills of a recipe catalog — the
// step-2 of two-step discovery (catalog → the recipes inside it).
// Reads the persisted catalog_member edges when the catalog lives in
// the dynamic index; otherwise derives membership from the catalog
// manifest's `allowed-tools` task grants (covers hub catalogs not
// indexed into the dynamic store + plain stores). Returns
// ErrSkillNotFound when the named skill doesn't exist, nil when it
// exists but is not a recipe catalog.
func (s *Store) CatalogMembers(ctx context.Context, name string) ([]Skill, error) {
	cat, err := s.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	if !cat.Manifest.Hugen.RecipeCatalog {
		return nil, nil
	}
	// Persisted-edge path (dynamic catalogs).
	if s.dynamic != nil {
		if members, err := s.dynamic.catalogMembersByName(ctx, name); err != nil {
			return nil, err
		} else if len(members) > 0 {
			return members, nil
		}
	}
	// Manifest-derived fallback: resolve each member recipe by name.
	var out []Skill
	for _, member := range catalogMemberNames(cat.Manifest) {
		sk, gerr := s.Get(ctx, member)
		if gerr != nil {
			continue // a named member that isn't installed — skip
		}
		out = append(out, sk)
	}
	return out, nil
}

// ApplyPins reconciles the advertise-pin flag across the dynamic index
// against the authoritative pin set (listed → pin=true, others →
// pin=false). No-op when no dynamic backend is wired. Invalidates the
// List cache.
func (s *Store) ApplyPins(ctx context.Context, pinNames []string) error {
	if s.dynamic == nil {
		return nil
	}
	if err := s.dynamic.applyPins(ctx, pinNames); err != nil {
		return err
	}
	s.Refresh()
	return nil
}

// Uninstall removes a dynamic skill's bundle + index row (the only
// explicit removal path). Returns ErrUnsupportedBackend when no
// dynamic backend is wired.
func (s *Store) Uninstall(ctx context.Context, name string) error {
	if s.dynamic == nil {
		return ErrUnsupportedBackend
	}
	if err := s.dynamic.Uninstall(ctx, name); err != nil {
		return err
	}
	s.Refresh()
	return nil
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
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	return Skill{
		Manifest: m,
		Origin:   b.origin,
		FS:       os.DirFS(dir),
		Root:     abs,
	}, nil
}

func (b *dirBackend) Publish(ctx context.Context, m Manifest, body fs.FS, opts PublishOptions) error {
	if !b.writable {
		return ErrUnsupportedBackend
	}
	if b.root == "" {
		return ErrUnsupportedBackend
	}
	dir := filepath.Join(b.root, m.Name)
	// Atomicity: build the bundle in a sibling tmp directory, then
	// swap it in via rename. Concurrent Get(name) reads see either
	// the previous version or the new one — never a half-written
	// state. If a previous Publish was interrupted leaving a tmp
	// directory behind, clear it on entry.
	tmpDir := dir + ".tmp"
	if err := os.RemoveAll(tmpDir); err != nil {
		return fmt.Errorf("clear stale %s: %w", tmpDir, err)
	}
	switch _, err := os.Stat(dir); {
	case err == nil:
		if !opts.Overwrite {
			return ErrSkillExists
		}
	case errors.Is(err, fs.ErrNotExist):
		// fresh directory — nothing to swap.
	default:
		return fmt.Errorf("stat %s: %w", dir, err)
	}
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", tmpDir, err)
	}
	// SKILL.md = original frontmatter (m.Raw) + body (m.Body) when
	// present; otherwise re-emit minimal frontmatter from the
	// validated manifest. Write into tmpDir; the atomic swap
	// happens at the end.
	manifestPath := filepath.Join(tmpDir, "SKILL.md")
	if err := os.WriteFile(manifestPath, encodeManifest(m), 0o644); err != nil {
		_ = os.RemoveAll(tmpDir)
		return fmt.Errorf("write %s: %w", manifestPath, err)
	}
	if body != nil {
		if err := copyFS(tmpDir, body); err != nil {
			_ = os.RemoveAll(tmpDir)
			return fmt.Errorf("copy body: %w", err)
		}
	}
	// Atomic swap: remove the existing dir (we're here only if
	// !exists or Overwrite=true) and rename tmpDir into place.
	// The window between RemoveAll and Rename is the only point
	// where a concurrent Get() can see ErrSkillNotFound; on
	// single-tenant local store this is acceptable. On the same
	// filesystem rename is atomic on POSIX.
	if err := os.RemoveAll(dir); err != nil {
		_ = os.RemoveAll(tmpDir)
		return fmt.Errorf("clear %s for swap: %w", dir, err)
	}
	if err := os.Rename(tmpDir, dir); err != nil {
		_ = os.RemoveAll(tmpDir)
		return fmt.Errorf("swap %s → %s: %w", tmpDir, dir, err)
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

func (b *inlineBackend) Publish(ctx context.Context, m Manifest, body fs.FS, opts PublishOptions) error {
	return ErrUnsupportedBackend
}

// --- embed-backed backend (system) ---

// embedBackend reads skills from an embed.FS (or any fs.FS).
// Used by OriginSystem to serve the agent-core skill set without
// touching disk. Mirrors dirBackend's manifest-loading logic but
// over fs.ReadDir / fs.ReadFile instead of os.* primitives.
type embedBackend struct {
	origin Origin
	fs     fs.FS
}

func (b *embedBackend) Origin() Origin { return b.origin }

func (b *embedBackend) List(ctx context.Context) ([]Skill, error) {
	if b.fs == nil {
		return nil, nil
	}
	entries, err := fs.ReadDir(b.fs, ".")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("embed: read root: %w", err)
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
		s, err := b.readSkill(e.Name())
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		out = append(out, s)
	}
	return out, errors.Join(errs...)
}

func (b *embedBackend) Get(ctx context.Context, name string) (Skill, error) {
	if b.fs == nil {
		return Skill{}, ErrSkillNotFound
	}
	if err := ctx.Err(); err != nil {
		return Skill{}, err
	}
	if info, err := fs.Stat(b.fs, name); err != nil || !info.IsDir() {
		return Skill{}, ErrSkillNotFound
	}
	return b.readSkill(name)
}

func (b *embedBackend) readSkill(name string) (Skill, error) {
	manifestPath := name + "/SKILL.md"
	content, err := fs.ReadFile(b.fs, manifestPath)
	if err != nil {
		return Skill{}, fmt.Errorf("read %s: %w", manifestPath, err)
	}
	m, err := Parse(content)
	if err != nil {
		return Skill{}, err
	}
	sub, err := fs.Sub(b.fs, name)
	if err != nil {
		return Skill{}, fmt.Errorf("sub %s: %w", name, err)
	}
	return Skill{
		Manifest: m,
		Origin:   b.origin,
		FS:       sub,
		// Root is the embed path; skill:files / skill:ref read
		// through the FS handle rather than touching disk.
		Root: "embed://" + name,
	}, nil
}

func (b *embedBackend) Publish(_ context.Context, _ Manifest, _ fs.FS, _ PublishOptions) error {
	return ErrUnsupportedBackend
}
