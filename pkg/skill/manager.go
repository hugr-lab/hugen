package skill

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
)

// SkillManager is the per-runtime registry of loaded skills,
// scoped per-Session. One instance per agent process; per-session
// state lives in sessionState.
//
// Bindings(sessionID) returns a per-Turn snapshot keyed by a
// monotonic generation counter; ToolManager rebuilds its
// catalogue when the generation moves. In-flight tool calls
// finish against the snapshot they started with.
type SkillManager struct {
	store SkillStore
	log   *slog.Logger

	mu       sync.RWMutex
	sessions map[string]*sessionState

	gen atomic.Int64

	subscribersMu sync.Mutex
	subscribers   []*subscriber
}

type sessionState struct {
	loaded   map[string]Skill // by manifest name
	gen      int64            // bumps on any change in this session
}

type subscriber struct {
	ctx context.Context
	ch  chan SkillChange
}

// SkillChange is what Subscribe streams. ToolManager listens to
// bump its policy_gen; adapters surface significant entries as
// audit Frames.
type SkillChange struct {
	Kind       SkillChangeKind
	SessionID  string // empty for global events (catalogue refresh, publish)
	SkillName  string
	Generation int64
	Err        error
}

type SkillChangeKind int

const (
	SkillLoaded SkillChangeKind = iota
	SkillUnloaded
	SkillRefreshed
	SkillPublished
	SkillRemoved
)

func (k SkillChangeKind) String() string {
	switch k {
	case SkillLoaded:
		return "loaded"
	case SkillUnloaded:
		return "unloaded"
	case SkillRefreshed:
		return "refreshed"
	case SkillPublished:
		return "published"
	case SkillRemoved:
		return "removed"
	default:
		return "unknown"
	}
}

// NewSkillManager constructs the registry. log may be nil for
// tests — the manager substitutes a discard logger.
func NewSkillManager(store SkillStore, log *slog.Logger) *SkillManager {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &SkillManager{
		store:    store,
		log:      log,
		sessions: make(map[string]*sessionState),
	}
}

// List returns every skill across every backend with their
// origins. Pass an empty filter to receive everything.
func (m *SkillManager) List(ctx context.Context) ([]Skill, error) {
	return m.store.List(ctx)
}

// Load resolves the metadata.hugen.requires closure for `name`
// and binds the resolved skills (the named one plus its
// transitive deps) to `sessionID`. Cycles return ErrSkillCycle.
// Unresolved skill references return a wrapped ErrSkillNotFound.
//
// Caller is responsible for separately checking that every
// granted tool's provider is registered with ToolManager —
// SkillManager only resolves skill→skill dependencies, not
// skill→tool grants.
func (m *SkillManager) Load(ctx context.Context, sessionID, name string) error {
	if sessionID == "" {
		return fmt.Errorf("skill: load: empty session id")
	}
	resolved, err := m.resolveClosure(ctx, name)
	if err != nil {
		return err
	}

	m.mu.Lock()
	st, ok := m.sessions[sessionID]
	if !ok {
		st = &sessionState{loaded: make(map[string]Skill)}
		m.sessions[sessionID] = st
	}
	for _, s := range resolved {
		st.loaded[s.Manifest.Name] = s
	}
	st.gen = m.gen.Add(1)
	m.mu.Unlock()

	for _, s := range resolved {
		m.emit(SkillChange{
			Kind:       SkillLoaded,
			SessionID:  sessionID,
			SkillName:  s.Manifest.Name,
			Generation: st.gen,
		})
	}
	return nil
}

// Unload removes `name` from the session. Idempotent — unloading
// a skill that was not loaded is not an error. Dependents
// (skills that listed `name` in metadata.hugen.requires) are NOT
// auto-unloaded; the caller decides cascade policy.
func (m *SkillManager) Unload(ctx context.Context, sessionID, name string) error {
	m.mu.Lock()
	st, ok := m.sessions[sessionID]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	if _, present := st.loaded[name]; !present {
		m.mu.Unlock()
		return nil
	}
	delete(st.loaded, name)
	st.gen = m.gen.Add(1)
	m.mu.Unlock()

	m.emit(SkillChange{
		Kind:       SkillUnloaded,
		SessionID:  sessionID,
		SkillName:  name,
		Generation: st.gen,
	})
	return nil
}

// Bindings returns the per-Turn snapshot for `sessionID`. The
// Generation token lets ToolManager rebuild its catalogue when
// it changes; callers MUST use the same Generation across all
// Turn-internal decisions to keep the snapshot stable.
func (m *SkillManager) Bindings(ctx context.Context, sessionID string) (Bindings, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st, ok := m.sessions[sessionID]
	if !ok {
		return Bindings{Generation: 0}, nil
	}
	out := Bindings{Generation: st.gen}
	memCats := map[string]MemoryCategory{}
	for _, s := range st.loaded {
		out.AllowedTools = append(out.AllowedTools, s.Manifest.AllowedTools...)
		out.SubAgentRoles = append(out.SubAgentRoles, s.Manifest.Hugen.SubAgents...)
		if s.Manifest.Hugen.MaxTurns > out.MaxTurns {
			out.MaxTurns = s.Manifest.Hugen.MaxTurns
		}
		for k, v := range s.Manifest.Hugen.Memory {
			memCats[k] = v
		}
		// Concatenate body content (skill instructions surfaced
		// to the system prompt). Order is map-iteration order
		// which is non-deterministic in Go — phase 4+ may want a
		// stable order; for phase 3 it's sufficient that the
		// contents are present.
		if len(s.Manifest.Body) > 0 {
			if out.Instructions != "" {
				out.Instructions += "\n\n"
			}
			out.Instructions += string(s.Manifest.Body)
		}
	}
	if len(memCats) > 0 {
		out.MemoryCategories = memCats
	}
	return out, nil
}

// UnavailableProvider tags a tool grant whose provider is not
// registered in the active ToolManager. Populated by
// AnnotateUnavailable on a Bindings snapshot — used by the
// no-Hugr (US5) deployment path so the model and operators can
// see which skills declared grants nobody is serving.
type UnavailableProvider struct {
	Provider string
	Tools    []string // tool patterns from the skill's allowed-tools
}

// Bindings is the per-Turn snapshot SkillManager hands ToolManager
// and the runtime's prompt builder.
type Bindings struct {
	Generation       int64
	Instructions     string                    // concatenated SKILL.md bodies
	AllowedTools     []ToolGrant               // union across loaded skills
	Unavailable      []UnavailableProvider     // grants whose provider is not registered (US5)
	SubAgentRoles    []SubAgentRole            // phase 4 dispatch source
	MemoryCategories map[string]MemoryCategory // for memory dispatch
	// MaxTurns is the largest metadata.hugen.max_turns across
	// every loaded skill. 0 when no skill specifies one — the
	// runtime then falls back to its default cap. Taking the max
	// is the principle of least surprise: an explorer skill
	// raising the budget shouldn't be undone by a co-loaded
	// utility skill keeping the default.
	MaxTurns int
}

// LoadedSkill returns the Skill named `name` if loaded into
// `sessionID`. Returns ErrSkillNotFound otherwise.
func (m *SkillManager) LoadedSkill(ctx context.Context, sessionID, name string) (Skill, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st, ok := m.sessions[sessionID]
	if !ok {
		return Skill{}, ErrSkillNotFound
	}
	s, ok := st.loaded[name]
	if !ok {
		return Skill{}, ErrSkillNotFound
	}
	return s, nil
}

// LoadedNames returns the names of every skill currently loaded
// for sessionID, sorted lexically. Empty slice (not nil) when
// the session has no skills loaded — distinguishable from "no
// skills configured" only by checking the slice length.
func (m *SkillManager) LoadedNames(_ context.Context, sessionID string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st, ok := m.sessions[sessionID]
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(st.loaded))
	for n := range st.loaded {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// Refresh re-reads `name` from the store and updates every
// session that has it loaded. Returns the new generation token.
func (m *SkillManager) Refresh(ctx context.Context, name string) (int64, error) {
	skill, err := m.store.Get(ctx, name)
	if err != nil {
		return 0, err
	}

	m.mu.Lock()
	gen := m.gen.Add(1)
	for _, st := range m.sessions {
		if _, ok := st.loaded[name]; ok {
			st.loaded[name] = skill
			st.gen = gen
		}
	}
	m.mu.Unlock()

	m.emit(SkillChange{
		Kind:       SkillRefreshed,
		SkillName:  name,
		Generation: gen,
	})
	return gen, nil
}

// RefreshAll re-reads every loaded skill across every session
// and bumps the generation.
func (m *SkillManager) RefreshAll(ctx context.Context) (int64, error) {
	m.mu.Lock()
	names := map[string]struct{}{}
	for _, st := range m.sessions {
		for n := range st.loaded {
			names[n] = struct{}{}
		}
	}
	m.mu.Unlock()

	gen := m.gen.Add(1)
	for n := range names {
		s, err := m.store.Get(ctx, n)
		if err != nil {
			m.log.Warn("skill: refresh failed", "name", n, "err", err)
			continue
		}
		m.mu.Lock()
		for _, st := range m.sessions {
			if _, ok := st.loaded[n]; ok {
				st.loaded[n] = s
				st.gen = gen
			}
		}
		m.mu.Unlock()
	}
	m.emit(SkillChange{Kind: SkillRefreshed, Generation: gen})
	return gen, nil
}

// Publish writes a skill to the writable backend (typically
// local://) via the underlying SkillStore. The freshly-published
// skill becomes discoverable via List/Get on the same store;
// consumers must Load it explicitly to bind it to a session.
func (m *SkillManager) Publish(ctx context.Context, manifest Manifest, body fs.FS) error {
	if err := m.store.Publish(ctx, manifest, body); err != nil {
		return err
	}
	m.emit(SkillChange{
		Kind:       SkillPublished,
		SkillName:  manifest.Name,
		Generation: m.gen.Add(1),
	})
	return nil
}

// Subscribe streams SkillChange events. The channel is closed
// when ctx is cancelled. A non-blocking send is used; subscribers
// that don't drain promptly will lose events.
func (m *SkillManager) Subscribe(ctx context.Context) (<-chan SkillChange, error) {
	ch := make(chan SkillChange, 16)
	sub := &subscriber{ctx: ctx, ch: ch}
	m.subscribersMu.Lock()
	m.subscribers = append(m.subscribers, sub)
	m.subscribersMu.Unlock()
	go func() {
		<-ctx.Done()
		m.subscribersMu.Lock()
		idx := slices.Index(m.subscribers, sub)
		if idx >= 0 {
			m.subscribers = slices.Delete(m.subscribers, idx, idx+1)
		}
		m.subscribersMu.Unlock()
		close(ch)
	}()
	return ch, nil
}

// emit fans an event out to every active subscriber. Slow
// subscribers drop events rather than block the producer.
func (m *SkillManager) emit(ev SkillChange) {
	m.subscribersMu.Lock()
	subs := slices.Clone(m.subscribers)
	m.subscribersMu.Unlock()
	for _, s := range subs {
		select {
		case s.ch <- ev:
		default:
			m.log.Warn("skill: dropping event to slow subscriber", "kind", ev.Kind, "skill", ev.SkillName)
		}
	}
}

// resolveClosure walks the metadata.hugen.requires graph from
// `root` outward, in DFS order, collecting every reachable skill.
// Cycles return ErrSkillCycle wrapping the cycle path. Returns
// the closure ordered so that dependencies precede dependents.
func (m *SkillManager) resolveClosure(ctx context.Context, root string) ([]Skill, error) {
	visited := map[string]bool{}
	visiting := map[string]bool{}
	var order []Skill

	var visit func(name, parent string) error
	visit = func(name, parent string) error {
		if visited[name] {
			return nil
		}
		if visiting[name] {
			return fmt.Errorf("%w: %s -> %s", ErrSkillCycle, parent, name)
		}
		visiting[name] = true
		s, err := m.store.Get(ctx, name)
		if err != nil {
			return fmt.Errorf("skill: load %s: %w", name, err)
		}
		for _, dep := range s.Manifest.Hugen.Requires {
			if err := visit(dep, name); err != nil {
				return err
			}
		}
		visiting[name] = false
		visited[name] = true
		order = append(order, s)
		return nil
	}

	if err := visit(root, ""); err != nil {
		return nil, err
	}
	return order, nil
}

// AnnotateUnavailable tags every allowed-tool grant in `b` whose
// provider is not present in `registered` as Unavailable. This is
// the US5 "warn-and-tag" surface: skills granting Hugr tools in a
// no-Hugr deployment stay loaded (so the model can still reason
// over the skill's instructions) but the runtime is honest about
// which tools won't dispatch.
//
// Returns a fresh Bindings copy; the input is not mutated.
func AnnotateUnavailable(b Bindings, registered []string) Bindings {
	if len(registered) == 0 {
		return b
	}
	known := make(map[string]struct{}, len(registered))
	for _, p := range registered {
		known[p] = struct{}{}
	}
	out := b
	out.AllowedTools = append([]ToolGrant(nil), b.AllowedTools...)
	out.Unavailable = nil
	for _, g := range b.AllowedTools {
		if _, ok := known[g.Provider]; ok {
			continue
		}
		out.Unavailable = append(out.Unavailable, UnavailableProvider{
			Provider: g.Provider,
			Tools:    append([]string(nil), g.Tools...),
		})
	}
	return out
}
