package scheduler

import (
	"context"
	"encoding/json"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// MaybeAutoApprove implements [extension.ToolApprovalPolicy] for
// cron-spawned sessions. The decision is purely "does this caller
// run under a cron fire whose pre-stamped allow-list includes
// `tool`?"; if yes the runtime skips the user-facing approval modal
// and the caller's per-fire tool call proceeds.
//
// Cron sessions do not have an interactive operator at fire time
// (Phase 6 §1.1 — that's the whole point of cron). The runtime
// therefore needs an explicit, per-tool authority decision the
// scheduler can hand down without prompting. The authority chain:
//
//  1. At task-create time the operator (or `_task_builder` mission)
//     pins a tool allow-list onto `tasks.spec.allowed_tools`.
//  2. The scheduler's fire fn stamps that list onto the cron
//     session's [protocol.FireContext.AllowedTools] when opening
//     the session.
//  3. The session state carries the FireContext under
//     [protocol.SchedulerFireStateKey]; this policy reads it on
//     each tool-approval inquiry.
//
// Skill-agnostic + cron-specific: contrast with the mission
// extension's auto-approve, which is mission-level "approve with
// tools" coming off an interactive plan-approval modal. The two
// can compose without overlap — a mission spawned inside a cron
// fire keeps the cron allow-list; an interactive mission ignores
// FireContext (it never finds one walking up).
//
// Phase 6 §0.5.6 / §3.5.
func (e *Extension) MaybeAutoApprove(ctx context.Context, caller extension.SessionState, tool string) (string, bool) {
	if caller == nil {
		return "", false
	}
	// Walk caller → root looking for the cron envelope. We stop at
	// the FIRST envelope encountered: nested cron-inside-cron is
	// undefined in Phase 6.1c (no path produces it today) but if it
	// ever happens the innermost task's allow-list wins, mirroring
	// the mission ext's "nearest ancestor with the flag" rule.
	for s := caller; s != nil; {
		if fc, ok := fireContextFromState(s); ok && fc != nil {
			if containsTool(fc.AllowedTools, tool) {
				e.emitToolAutoGranted(s, toolAutoGrantedPayload{
					Tool:                  tool,
					CallerSessionID:       caller.SessionID(),
					GrantedByCronSession:  s.SessionID(),
					GrantedByTaskID:       fc.TaskID,
					GrantedByFireSeq:      fc.FireSeq,
				})
				return fc.TaskID, true
			}
			// Envelope present but tool not on the list — explicit
			// deny: keep walking would only confuse audit. Cron is
			// a single-authority chain.
			return "", false
		}
		parent, ok := s.Parent()
		if !ok || parent == nil {
			return "", false
		}
		s = parent
	}
	return "", false
}

// fireContextFromState returns the cron envelope stamped on state
// by the session constructor when [session.OpenRequest.Cron] was
// non-nil. Returns (nil, false) on root / subagent sessions.
func fireContextFromState(state extension.SessionState) (*protocol.FireContext, bool) {
	v, ok := state.Value(protocol.SchedulerFireStateKey)
	if !ok {
		return nil, false
	}
	fc, ok := v.(*protocol.FireContext)
	if !ok || fc == nil {
		return nil, false
	}
	return fc, true
}

func containsTool(allow []string, name string) bool {
	for _, t := range allow {
		if t == name {
			return true
		}
	}
	return false
}

// toolAutoGrantedPayload is the body of the
// `scheduler:tool_auto_granted_by_task` ExtensionFrame — one per
// implicit grant, anchored on the cron session so a per-fire audit
// scan groups every grant. Default-deny in visibility filters;
// audit-only.
type toolAutoGrantedPayload struct {
	// Tool is the full provider-qualified tool name.
	Tool string `json:"tool"`

	// CallerSessionID names the session whose tool call was
	// about to surface an approval modal. May be the cron
	// session itself (Phase 6.1c: workers inside cron land in
	// Phase 6.2).
	CallerSessionID string `json:"caller_session_id"`

	// GrantedByCronSession is the cron session id under whose
	// FireContext the allow-list lived. Equals the frame's
	// SessionID; duplicated here for self-contained payloads.
	GrantedByCronSession string `json:"granted_by_cron_session"`

	// GrantedByTaskID is the source task row id.
	GrantedByTaskID string `json:"granted_by_task_id"`

	// GrantedByFireSeq is the fire counter under which the
	// grant fired.
	GrantedByFireSeq int `json:"granted_by_fire_seq"`
}

// emitToolAutoGranted publishes the audit frame on the cron
// session (not the caller's session) so a per-fire audit scan
// groups every grant under one envelope. Failure is logged at
// debug — the dispatch must not block on observability.
func (e *Extension) emitToolAutoGranted(cron extension.SessionState, payload toolAutoGrantedPayload) {
	data, err := json.Marshal(payload)
	if err != nil {
		e.logger.Warn("scheduler: emit tool_auto_granted: marshal failed",
			"cron_session", cron.SessionID(), "tool", payload.Tool, "err", err)
		return
	}
	frame := protocol.NewExtensionFrame(
		cron.SessionID(),
		schedulerParticipant(e.agentID),
		providerName,
		protocol.CategoryOp,
		"tool_auto_granted_by_task",
		data,
	)
	if err := cron.Emit(context.Background(), frame); err != nil {
		e.logger.Warn("scheduler: emit tool_auto_granted: emit failed",
			"cron_session", cron.SessionID(), "tool", payload.Tool, "err", err)
	}
}

// schedulerParticipant is the runtime-agent participant attached
// to scheduler-authored frames. Kept private; callers go through
// the helper to keep the ID stable across emit sites.
func schedulerParticipant(agentID string) protocol.ParticipantInfo {
	return protocol.ParticipantInfo{ID: agentID, Kind: protocol.ParticipantAgent, Name: "hugen"}
}
