package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// initSubagent registers the five sub-agent orchestration tools
// into the per-session dispatch table set up in tools_provider.go.
// Per phase-4-spec §15 step 7 + contracts/tools-subagent.md these
// surface to the LLM as session:spawn_subagent /
// session:wait_subagents / session:subagent_runs /
// session:subagent_cancel / session:parent_context.
//
// Each tool's schema, payload types, and handler live in its own
// tools_subagent_<name>.go file; this shell only wires the
// dispatch table + the helpers (describeSubagent /
// hasSubagentDescriber / assertChildOf) every handler shares.
func (s *Session) initSubagent() {
	s.sessionTools["spawn_subagent"] = sessionToolDescriptor{
		Name:             "spawn_subagent",
		Description:      "Spawn one or more sub-agent sessions. Non-blocking — returns child session ids immediately; results arrive asynchronously via wait_subagents.",
		PermissionObject: permObjectSubagentSpawn,
		ArgSchema:        json.RawMessage(spawnSubagentSchema),
		Handler:          s.callSpawnSubagent,
	}
	s.sessionTools["wait_subagents"] = sessionToolDescriptor{
		Name:             "wait_subagents",
		Description:      "Block until each listed sub-agent produces a terminal result. Returns one row per id.",
		PermissionObject: permObjectSubagentWait,
		ArgSchema:        json.RawMessage(waitSubagentsSchema),
		Handler:          s.callWaitSubagents,
	}
	s.sessionTools["subagent_runs"] = sessionToolDescriptor{
		Name:             "subagent_runs",
		Description:      "Paginated transcript pull-through for a sub-agent the calling session spawned.",
		PermissionObject: permObjectSubagentRead,
		ArgSchema:        json.RawMessage(subagentRunsSchema),
		Handler:          s.callSubagentRuns,
	}
	s.sessionTools["subagent_cancel"] = sessionToolDescriptor{
		Name:             "subagent_cancel",
		Description:      "Cancel one of the calling session's sub-agents with a stated reason. Cascades to descendants via ctx.",
		PermissionObject: permObjectSubagentCancel,
		ArgSchema:        json.RawMessage(subagentCancelSchema),
		Handler:          s.callSubagentCancel,
	}
	s.sessionTools["parent_context"] = sessionToolDescriptor{
		Name:             "parent_context",
		Description:      "Sub-agent's window into its direct parent's user-facing communication. Filtered to user/assistant messages.",
		PermissionObject: permObjectSubagentParentContext,
		ArgSchema:        json.RawMessage(parentContextSchema),
		Handler:          s.callParentContext,
	}
}

// Permission objects per contracts/permission-objects.md §"Sub-agent
// system tools". These names are baked into the Tool descriptors
// returned by Manager.List so the 3-tier permission stack can gate
// each tool independently.
const (
	permObjectSubagentSpawn         = "hugen:subagent:spawn"
	permObjectSubagentWait          = "hugen:subagent:wait"
	permObjectSubagentRead          = "hugen:subagent:read"
	permObjectSubagentCancel        = "hugen:subagent:cancel"
	permObjectSubagentParentContext = "hugen:subagent:parent_context"
)

// describeSubagent walks every [extension.SubagentDescriber] on
// parent and composes the per-advisor verdicts: SubagentValid from
// any advisor wins; otherwise SubagentSkillFoundRoleMissing wins
// over SubagentUnknown. Errors from any advisor are returned
// immediately — failing fast on a corrupt skill catalog matches
// the pre-stage-4 lookupSubAgentRole behaviour.
func describeSubagent(ctx context.Context, parent *Session, skillName, roleName string) (extension.SubagentValidation, error) {
	if parent.deps == nil {
		return extension.SubagentValid, nil
	}
	best := extension.SubagentUnknown
	for _, ext := range parent.deps.Extensions {
		adv, ok := ext.(extension.SubagentDescriber)
		if !ok {
			continue
		}
		got, err := adv.DescribeSubagent(ctx, parent, skillName, roleName)
		if err != nil {
			return extension.SubagentUnknown, err
		}
		switch got {
		case extension.SubagentValid:
			return extension.SubagentValid, nil
		case extension.SubagentSkillFoundRoleMissing:
			best = extension.SubagentSkillFoundRoleMissing
		}
	}
	return best, nil
}

// hasSubagentDescriber reports whether any registered extension
// implements [extension.SubagentDescriber]. Used by the spawn
// validator to distinguish "no advisor → no validation" (legacy
// behaviour for fixture / no-skill tests) from "advisor present
// but didn't recognise the skill → skill_not_found".
func hasSubagentDescriber(parent *Session) bool {
	if parent.deps == nil {
		return false
	}
	for _, ext := range parent.deps.Extensions {
		if _, ok := ext.(extension.SubagentDescriber); ok {
			return true
		}
	}
	return false
}

// assertChildOf returns a tool_error JSON when sessionID is not a
// direct child of the calling session (or doesn't exist). Used by
// subagent_runs + subagent_cancel to gate cross-session reads.
func (parent *Session) assertChildOf(ctx context.Context, sessionID string) json.RawMessage {
	if sessionID == parent.id {
		out, _ := toolErr("not_a_child", "session_id is the caller itself")
		return out
	}
	row, err := parent.store.LoadSession(ctx, sessionID)
	if err != nil {
		if errors.Is(err, store.ErrSessionNotFound) {
			out, _ := toolErr("session_not_found", fmt.Sprintf("session %q not found", sessionID))
			return out
		}
		out, _ := toolErr("io", err.Error())
		return out
	}
	if row.ParentSessionID != parent.id {
		out, _ := toolErr("not_a_child",
			fmt.Sprintf("session %q is not a child of %q", sessionID, parent.id))
		return out
	}
	return nil
}
