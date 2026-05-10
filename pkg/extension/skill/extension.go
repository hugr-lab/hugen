// Package skill is the agent-level extension wrapping
// [*skill.SkillManager] + [perm.Service] for use as a session
// extension. Through stages 1-5 of phase 4.1b-pre this package
// absorbed every skill-related concern that used to live in
// pkg/session: the tool catalogue (load/unload/files/ref/publish/
// tools_catalog), the system-prompt sections, the per-session
// allow-list filter, the slash command handler, the policy advisor,
// the subagent role describer, the per-session loaded-skills state
// itself, and the Recovery / Closer hooks.
package skill

import (
	"context"
	"sort"
	"sync"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
	skillpkg "github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// StateKey is the [extension.SessionState] key the extension stores
// its per-session [*SessionSkill] handle under. Exported so callers
// can recover the handle without magic strings.
const StateKey = "skill"

// providerName is the catalogue prefix the LLM sees: "skill:<tool>".
// Doubles as [Extension.Name] and (transitively) the [StateKey].
const providerName = "skill"

// Extension wires [*skill.SkillManager] + [perm.Service] into the
// session capability pipeline. The instance is shared across every
// session under one Manager; per-session state lives in
// [extension.SessionState] under [StateKey].
type Extension struct {
	manager *skillpkg.SkillManager
	perms   perm.Service
	agentID string
}

// agentParticipant returns the ParticipantInfo skill ext stamps on
// every emitted ExtensionFrame. The skill tool path is always
// invoked by the agent (the model issues the tool call), so author
// is the agent — operator / human authors flow through other
// surfaces (slash commands).
func (e *Extension) agentParticipant() protocol.ParticipantInfo {
	return protocol.ParticipantInfo{ID: e.agentID, Kind: protocol.ParticipantAgent}
}

// NewExtension builds the skill extension. sm is the agent-level
// SkillManager; perms is the shared permission service (used to
// gate skill:files); agentID stamps any future event-log writes.
func NewExtension(sm *skillpkg.SkillManager, perms perm.Service, agentID string) *Extension {
	return &Extension{manager: sm, perms: perms, agentID: agentID}
}

// Compile-time interface assertions.
var (
	_ extension.Extension        = (*Extension)(nil)
	_ extension.StateInitializer = (*Extension)(nil)
	_ tool.ToolProvider          = (*Extension)(nil)
)

// Name implements [extension.Extension] and [tool.ToolProvider].
func (e *Extension) Name() string { return providerName }

// Lifetime implements [tool.ToolProvider]. The provider is stateless
// (per-session state lives in [extension.SessionState]) so PerAgent
// — one provider instance shared across sessions.
func (e *Extension) Lifetime() tool.Lifetime { return tool.LifetimePerAgent }

// InitState implements [extension.StateInitializer]. Allocates a
// fresh [SessionSkill] handle for the calling session, stashes it
// under [StateKey], registers it as a [skillpkg.SessionSink] so
// manager-level Refresh broadcasts find it, and runs autoload
// (skills with metadata.hugen.autoload_in matching this session's
// derived type — root or subagent based on parent linkage).
func (e *Extension) InitState(ctx context.Context, state extension.SessionState) error {
	h := &SessionSkill{
		manager:     e.manager,
		perms:       e.perms,
		sessionID:   state.SessionID(),
		author:      e.agentParticipant(),
		loaded:      map[string]skillpkg.Skill{},
		sessionType: deriveSessionType(state),
	}
	state.SetValue(StateKey, h)
	if e.manager == nil {
		return nil
	}
	e.manager.RegisterSink(h)
	h.autoload(ctx)
	return nil
}

// deriveSessionType picks SessionType from the session's parent
// linkage: a session with no parent is the agent's root session;
// anything spawned via Spawn is a subagent. Mirrors the
// constructors.go classification that ends up on store.SessionRow.
func deriveSessionType(state extension.SessionState) string {
	if _, hasParent := state.Parent(); hasParent {
		return skillpkg.SessionTypeSubAgent
	}
	return skillpkg.SessionTypeRoot
}

// autoload binds every skill that opts into autoload for this
// session's derived type. Per-skill failures log via the
// manager's logger and continue; one bad bundle must not deny the
// session its working tool surface.
func (h *SessionSkill) autoload(ctx context.Context) {
	if h.manager == nil {
		return
	}
	all, err := h.manager.List(ctx)
	if err != nil {
		return
	}
	for _, sk := range all {
		if !sk.Manifest.AutoloadIn(h.sessionType) {
			continue
		}
		_ = h.Load(ctx, sk.Manifest.Name)
	}
}

// SessionSkill is the per-session typed handle stashed in
// [extension.SessionState] under [StateKey]. Owns the loaded
// skills set + the per-session generation counter — the state
// pkg/skill.SkillManager used to keep in its m.sessions map until
// stage 5 of phase 4.1b-pre dissolved that field.
type SessionSkill struct {
	manager     *skillpkg.SkillManager
	perms       perm.Service
	sessionID   string
	author      protocol.ParticipantInfo
	sessionType string // skill.SessionTypeRoot / SessionTypeSubAgent

	mu     sync.RWMutex
	loaded map[string]skillpkg.Skill // by manifest name
	gen    int64                     // per-session generation; tracks manager.Gen() at last mutation
}

// Compile-time assertion that *SessionSkill satisfies the manager's
// sink contract.
var _ skillpkg.SessionSink = (*SessionSkill)(nil)

// Load resolves the metadata.hugen.requires closure for `name`
// via the manager and binds the resolved skills to this session.
// Cycles return ErrSkillCycle; unresolved skill references return
// a wrapped ErrSkillNotFound.
func (h *SessionSkill) Load(ctx context.Context, name string) error {
	if h.manager == nil {
		return tool.ErrSystemUnavailable
	}
	resolved, err := h.manager.ResolveClosure(ctx, name)
	if err != nil {
		return err
	}
	h.mu.Lock()
	for _, s := range resolved {
		h.loaded[s.Manifest.Name] = s
	}
	h.gen = h.manager.BumpGen()
	gen := h.gen
	h.mu.Unlock()
	for _, s := range resolved {
		h.manager.EmitChange(skillpkg.SkillChange{
			Kind:       skillpkg.SkillLoaded,
			SessionID:  h.sessionID,
			SkillName:  s.Manifest.Name,
			Generation: gen,
		})
	}
	return nil
}

// Unload removes `name` from the session. Idempotent — unloading
// a skill that was not loaded is not an error.
func (h *SessionSkill) Unload(_ context.Context, name string) error {
	h.mu.Lock()
	if _, present := h.loaded[name]; !present {
		h.mu.Unlock()
		return nil
	}
	delete(h.loaded, name)
	if h.manager != nil {
		h.gen = h.manager.BumpGen()
	} else {
		h.gen++
	}
	gen := h.gen
	h.mu.Unlock()
	if h.manager != nil {
		h.manager.EmitChange(skillpkg.SkillChange{
			Kind:       skillpkg.SkillUnloaded,
			SessionID:  h.sessionID,
			SkillName:  name,
			Generation: gen,
		})
	}
	return nil
}

// Bindings returns the per-Turn snapshot for this session. The
// Generation token lets the snapshot cache invalidate when it
// changes; callers MUST use the same Generation across all
// turn-internal decisions to keep the snapshot stable.
func (h *SessionSkill) Bindings(_ context.Context) (skillpkg.Bindings, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := skillpkg.Bindings{Generation: h.gen}
	memCats := map[string]skillpkg.MemoryCategory{}
	// Iterate `loaded` in name-sorted order — random map order
	// would change the prompt byte-for-byte each Turn, defeating
	// the upstream prompt-cache (Anthropic / OpenAI key cache by
	// prefix bytes).
	names := make([]string, 0, len(h.loaded))
	for n := range h.loaded {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		s := h.loaded[n]
		// Tri-state AllowedTools: nil means "absent" (skill inherits
		// the union of other loaded skills' explicit grants — falls
		// out naturally because admission tests against the
		// aggregated explicit-grants list); non-nil empty means
		// "reference-only"; populated contributes those. Both
		// nil and empty contribute nothing to the union here.
		// See manifest.go::Manifest.AllowedTools.
		out.AllowedTools = append(out.AllowedTools, s.Manifest.AllowedTools...)
		out.SubAgentRoles = append(out.SubAgentRoles, s.Manifest.Hugen.SubAgents...)
		if s.Manifest.Hugen.MaxTurns > out.MaxTurns {
			out.MaxTurns = s.Manifest.Hugen.MaxTurns
		}
		if s.Manifest.Hugen.MaxTurnsHard > out.MaxTurnsHard {
			out.MaxTurnsHard = s.Manifest.Hugen.MaxTurnsHard
		}
		if !s.Manifest.Hugen.StuckDetection.IsEnabled() {
			out.StuckDetectionDisabled = true
		}
		for k, v := range s.Manifest.Hugen.Memory {
			memCats[k] = v
		}
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

// LoadedSkill returns the Skill named `name` if loaded into this
// session. Returns ErrSkillNotFound otherwise.
func (h *SessionSkill) LoadedSkill(_ context.Context, name string) (skillpkg.Skill, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	s, ok := h.loaded[name]
	if !ok {
		return skillpkg.Skill{}, skillpkg.ErrSkillNotFound
	}
	return s, nil
}

// LoadedNames returns the names of every skill currently loaded
// for this session, sorted lexically. Empty slice (not nil) when
// no skills are loaded.
func (h *SessionSkill) LoadedNames(_ context.Context) []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]string, 0, len(h.loaded))
	for n := range h.loaded {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// OnSkillRefreshed implements [skillpkg.SessionSink]. Called by
// the manager when Refresh / RefreshAll re-reads `s` from the
// store; the handle replaces its in-memory copy of `s` if it had
// it loaded and bumps its per-session generation so the snapshot
// cache invalidates.
func (h *SessionSkill) OnSkillRefreshed(s skillpkg.Skill) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.loaded[s.Manifest.Name]; !ok {
		return
	}
	h.loaded[s.Manifest.Name] = s
	if h.manager != nil {
		h.gen = h.manager.Gen()
	} else {
		h.gen++
	}
}

// emitOp is the shared persist path for the tool-side load /
// unload events. Builds the matching ExtensionFrame and pushes it
// through the calling session's [extension.SessionState.Emit] so
// Recovery can replay it on restart. Errors are logged-but-not-
// returned: the in-memory mutation already happened, and dropping
// a persistence write surfaces as a missing replay event rather
// than a tool-call failure (the model's view stays consistent
// within the live process).
func (h *SessionSkill) emitOp(ctx context.Context, op, name string) {
	state, ok := extension.SessionStateFromContext(ctx)
	if !ok || state == nil {
		return
	}
	var (
		frame protocol.Frame
		err   error
	)
	switch op {
	case OpLoad:
		frame, err = newLoadFrame(h.sessionID, h.author, name)
	case OpUnload:
		frame, err = newUnloadFrame(h.sessionID, h.author, name)
	default:
		return
	}
	if err != nil {
		return
	}
	_ = state.Emit(ctx, frame)
}

// Manager returns the shared *SkillManager.
func (h *SessionSkill) Manager() *skillpkg.SkillManager { return h.manager }

// SessionID returns the session id this handle is bound to.
func (h *SessionSkill) SessionID() string { return h.sessionID }

// FromState returns the per-session [*SessionSkill] handle, or nil
// if the extension hasn't run InitState for the calling session
// (e.g. tests that build a session without registering the
// extension).
func FromState(state extension.SessionState) *SessionSkill {
	if state == nil {
		return nil
	}
	v, ok := state.Value(StateKey)
	if !ok {
		return nil
	}
	h, _ := v.(*SessionSkill)
	return h
}
