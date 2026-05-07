// Package skill is the agent-level extension wrapping
// [*skill.SkillManager] + [perm.Service] for use as a session
// extension. Phase 4.1b-pre stage 1: the five skill_* tools migrate
// off pkg/session/tools_skills.go onto a [tool.ToolProvider]
// implemented by this package; subsequent stages fold in
// Advertiser, ToolFilter, Recovery, Closer, GenerationProvider, and
// Commander capabilities so all skill-related code lives here.
package skill

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/extension"
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
// fresh [SessionSkill] handle for the calling session and stashes
// it under [StateKey].
func (e *Extension) InitState(_ context.Context, state extension.SessionState) error {
	state.SetValue(StateKey, &SessionSkill{
		manager:   e.manager,
		perms:     e.perms,
		sessionID: state.SessionID(),
	})
	return nil
}

// SessionSkill is the per-session typed handle stashed in
// [extension.SessionState] under [StateKey]. Handlers and future
// capabilities (advertiser, filter, recovery, closer) recover it
// via [FromState].
type SessionSkill struct {
	manager   *skillpkg.SkillManager
	perms     perm.Service
	sessionID string
}

// Manager returns the shared *SkillManager. Exported so future
// helpers (subagent.RoleFor, resolveToolIterCap) can read manifests
// + bindings via the same handle.
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
