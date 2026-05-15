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

// SkillManager is the agent-level façade over a [SkillStore]. It
// reads / refreshes / publishes skill manifests and broadcasts
// catalog-level events to registered sinks; it does NOT own
// per-session state.
//
// Per-session state — which skills are loaded, the rendered
// Bindings snapshot, the per-session generation counter — lives on
// the session-scoped [github.com/hugr-lab/hugen/pkg/extension/skill.SessionSkill]
// handle (the skill extension's per-session projection). Sinks
// register / deregister via [RegisterSink] so the manager can
// broadcast Refresh events without owning session state.
type SkillManager struct {
	store SkillStore
	log   *slog.Logger

	// gen is the agent-level monotonic counter the manager bumps on
	// every Refresh / Publish. Per-session handles read it via
	// [BumpGen] / [Gen]; their own Bindings.Generation rides this
	// value so the snapshot cache key invalidates correctly.
	gen atomic.Int64

	sinksMu sync.Mutex
	sinks   []SessionSink

	subscribersMu sync.Mutex
	subscribers   []*subscriber
}

// SessionSink is the narrow callback surface the manager invokes
// on Refresh / RefreshAll. The skill extension's per-session
// handle implements it; pkg/skill imports nothing from the
// extension package.
type SessionSink interface {
	// SessionID identifies the sink for deregistration.
	SessionID() string

	// OnSkillRefreshed is called when [SkillManager.Refresh] /
	// [SkillManager.RefreshAll] re-reads `skill` from the store.
	// Sinks should update any in-memory copy of the skill they
	// hold for this session and bump their per-session generation
	// so the next Bindings call sees the fresh manifest.
	OnSkillRefreshed(skill Skill)
}

type subscriber struct {
	ctx context.Context
	ch  chan SkillChange
}

// SkillChange is what Subscribe streams. Adapters surface
// significant entries as audit Frames.
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

// NewSkillManager constructs the manager. log may be nil for
// tests — the manager substitutes a discard logger.
func NewSkillManager(store SkillStore, log *slog.Logger) *SkillManager {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &SkillManager{store: store, log: log}
}

// List returns every skill across every backend with their
// origins.
func (m *SkillManager) List(ctx context.Context) ([]Skill, error) {
	return m.store.List(ctx)
}

// Get returns the skill named `name` from the store. Returns
// ErrSkillNotFound when no backend has it.
func (m *SkillManager) Get(ctx context.Context, name string) (Skill, error) {
	return m.store.Get(ctx, name)
}

// Gen returns the current agent-level generation counter — the
// monotonic stamp [SkillManager.Refresh] / [SkillManager.Publish]
// bumps. Per-session handles use this to compute their own
// Bindings.Generation token.
func (m *SkillManager) Gen() int64 { return m.gen.Load() }

// BumpGen increments the agent-level generation counter and
// returns the new value. Per-session handles call this on every
// Load / Unload that mutates their loaded set.
func (m *SkillManager) BumpGen() int64 { return m.gen.Add(1) }

// EmitChange broadcasts ev to every active Subscribe channel.
// Per-session handles call this after Load / Unload so adapters
// see the same audit-shaped events the legacy SkillManager.Load
// emitted; the manager itself fires it on Refresh / Publish.
func (m *SkillManager) EmitChange(ev SkillChange) { m.emit(ev) }

// ResolveClosure walks the metadata.hugen.requires graph from
// `root` outward, in DFS order, collecting every reachable skill.
// Cycles return ErrSkillCycle wrapping the cycle path. Returns
// the closure ordered so dependencies precede dependents.
//
// Per-session handlers call this once per Load to fetch the
// transitive set of skills they should bind into the session.
func (m *SkillManager) ResolveClosure(ctx context.Context, root string) ([]Skill, error) {
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
		for _, dep := range s.Manifest.Hugen.AllRequires() {
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

// RegisterSink adds sink to the broadcast list. Idempotent for
// the same SessionID — a re-register replaces the prior entry so
// extensions can re-init without duplicate callbacks. Called
// from skill extension InitState.
func (m *SkillManager) RegisterSink(sink SessionSink) {
	if sink == nil {
		return
	}
	m.sinksMu.Lock()
	defer m.sinksMu.Unlock()
	id := sink.SessionID()
	for i, s := range m.sinks {
		if s.SessionID() == id {
			m.sinks[i] = sink
			return
		}
	}
	m.sinks = append(m.sinks, sink)
}

// DeregisterSink removes the sink for sessionID. Idempotent.
// Called from skill extension CloseSession.
func (m *SkillManager) DeregisterSink(sessionID string) {
	m.sinksMu.Lock()
	defer m.sinksMu.Unlock()
	for i, s := range m.sinks {
		if s.SessionID() == sessionID {
			m.sinks = slices.Delete(m.sinks, i, i+1)
			return
		}
	}
}

// snapshotSinks returns a defensive copy of the sink list under
// lock; callers iterate without holding the manager lock.
func (m *SkillManager) snapshotSinks() []SessionSink {
	m.sinksMu.Lock()
	defer m.sinksMu.Unlock()
	return slices.Clone(m.sinks)
}

// Refresh re-reads `name` from the store, broadcasts the fresh
// Skill to every registered SessionSink, and emits a
// SkillRefreshed event. Returns the new generation token.
func (m *SkillManager) Refresh(ctx context.Context, name string) (int64, error) {
	skill, err := m.store.Get(ctx, name)
	if err != nil {
		return 0, err
	}
	gen := m.gen.Add(1)
	for _, sink := range m.snapshotSinks() {
		sink.OnSkillRefreshed(skill)
	}
	m.emit(SkillChange{
		Kind:       SkillRefreshed,
		SkillName:  name,
		Generation: gen,
	})
	return gen, nil
}

// RefreshAll re-reads every skill the store knows about and
// broadcasts each to every sink. Mismatched skills (in the store
// but not loaded by any sink) cost only one Get; the sink decides
// whether the refresh is relevant.
func (m *SkillManager) RefreshAll(ctx context.Context) (int64, error) {
	all, err := m.store.List(ctx)
	if err != nil {
		return 0, err
	}
	gen := m.gen.Add(1)
	sinks := m.snapshotSinks()
	for _, s := range all {
		fresh, gerr := m.store.Get(ctx, s.Manifest.Name)
		if gerr != nil {
			m.log.Warn("skill: refresh failed", "name", s.Manifest.Name, "err", gerr)
			continue
		}
		for _, sink := range sinks {
			sink.OnSkillRefreshed(fresh)
		}
	}
	m.emit(SkillChange{Kind: SkillRefreshed, Generation: gen})
	return gen, nil
}

// Publish writes a skill to the writable backend (typically
// local://) via the underlying SkillStore. The freshly-published
// skill becomes discoverable via List/Get on the same store;
// consumers must Load it explicitly to bind it to a session.
//
// PublishOptions.Overwrite=false (default) returns ErrSkillExists
// on collision; set Overwrite=true within the skill:save
// validation iteration loop only.
func (m *SkillManager) Publish(ctx context.Context, manifest Manifest, body fs.FS, opts PublishOptions) error {
	if err := m.store.Publish(ctx, manifest, body, opts); err != nil {
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

// UnavailableProvider tags a tool grant whose provider is not
// registered in the active ToolManager. Populated by
// AnnotateUnavailable on a Bindings snapshot — used by the
// no-Hugr (US5) deployment path so the model and operators can
// see which skills declared grants nobody is serving.
type UnavailableProvider struct {
	Provider string
	Tools    []string // tool patterns from the skill's allowed-tools
}

// Bindings is the per-Turn snapshot the skill extension hands the
// runtime's prompt builder. Composed on the per-session
// [SessionSkill] handle from the manager's loaded skills. Phase
// 5.2 δ dropped the turn-loop budget fields (MaxTurns / MaxTurnsHard
// / StuckDetectionDisabled) — those resolve through
// [github.com/hugr-lab/hugen/pkg/extension.TurnBudgetLookup] +
// `config.subagents.tier_defaults.<tier>` now.
type Bindings struct {
	Generation       int64
	Instructions     string                    // concatenated SKILL.md bodies
	AllowedTools     []ToolGrant               // union across loaded skills
	Unavailable      []UnavailableProvider     // grants whose provider is not registered (US5)
	SubAgentRoles    []SubAgentRole            // phase 4 dispatch source
	MemoryCategories map[string]MemoryCategory // for memory dispatch
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
