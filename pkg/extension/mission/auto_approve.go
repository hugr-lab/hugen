package mission

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/extension"
)

// MaybeAutoApprove implements [extension.ToolApprovalPolicy] for the
// mission extension. Walks the caller's parent chain looking for an
// ancestor whose MissionState has AutoApproveTools=true; on hit,
// emits a mission:tool_approval_auto_granted audit frame and
// returns (missionID, true) so the runtime skips the user-facing
// approval modal for this tool call.
//
// Skill-agnostic: nothing here knows about a particular skill or
// tool name. The decision is purely "did some ancestor mission opt
// into auto-approve on its own plan-approval modal?" — that's the
// data contract §4.6 spells out.
//
// Returns ("", false) when:
//   - caller is nil or has no mission ancestor
//   - the nearest mission ancestor has AutoApproveTools=false
//
// Concurrency: MissionState.AutoApproveTools is mutex-guarded;
// concurrent dispatch + modal close cannot tear the read.
//
// Phase 5.x — §4.6.5.
func (e *Extension) MaybeAutoApprove(ctx context.Context, caller extension.SessionState, tool string) (string, bool) {
	if caller == nil {
		return "", false
	}
	// Walk ancestors. The caller itself may be a mission session
	// (root-as-chat or nested missions); we still consult it, since
	// the flag lives on whichever ancestor approved the plan.
	for s := caller; s != nil; {
		if v, ok := s.Value(StateKey); ok {
			if m, _ := v.(*MissionState); m != nil && m.AutoApproveTools() {
				missionID := s.SessionID()
				e.emitToolApprovalAutoGranted(s, toolApprovalAutoGrantedPayload{
					Tool:                      tool,
					CallerSessionID:           caller.SessionID(),
					GrantedByMissionSessionID: missionID,
				})
				return missionID, true
			}
		}
		parent, ok := s.Parent()
		if !ok || parent == nil {
			return "", false
		}
		s = parent
	}
	return "", false
}

// toolApprovalAutoGrantedPayload is the body of the
// `mission:tool_approval_auto_granted` ExtensionFrame — written once
// per implicit grant so the audit log carries the full sequence of
// "auto-approve fired on tool X under mission Y". Default-deny in
// visibility filters; audit-only. Phase 5.x — §4.6.4.
type toolApprovalAutoGrantedPayload struct {
	// Tool is the full provider-qualified tool name
	// ("<provider>:<short>") whose approval inquiry was skipped.
	Tool string `json:"tool"`

	// CallerSessionID identifies the session that would have
	// opened the modal — typically a worker session under the
	// granting mission. The frame is emitted on the granting
	// mission session (not the caller) so a per-mission audit
	// scan groups every grant in one place.
	CallerSessionID string `json:"caller_session_id"`

	// GrantedByMissionSessionID is the mission session whose
	// MissionState.AutoApproveTools=true matched on the walk.
	// Emitted as the frame's SessionID + duplicated here so the
	// payload is self-contained.
	GrantedByMissionSessionID string `json:"granted_by_mission_session_id"`
}

// emitToolApprovalAutoGranted publishes the audit frame on the
// mission session (NOT the caller's session) so a per-mission audit
// scan groups every grant in one place. Reuses the common
// emitMissionOp helper for consistent envelope wiring.
func (e *Extension) emitToolApprovalAutoGranted(mission extension.SessionState, payload toolApprovalAutoGrantedPayload) {
	e.emitMissionOp(mission, "tool_approval_auto_granted", payload)
}
