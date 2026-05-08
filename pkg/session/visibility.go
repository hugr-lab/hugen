package session

import (
	"fmt"

	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// visibility.go implements the §11 frame-visibility filter applied at
// the pendingInbound drain layer. Every Frame that enters
// pendingInbound is persisted (event-source rule), but only an explicit
// allow-list reaches s.history (model-visible). Default is deny — a
// new Frame Kind in a future phase MUST add itself to projectFrameToHistory
// or stay invisible to the model on this session.
//
// RouteInternal Frames are out-of-scope: their handlers persist their
// own derived events directly. RouteToolFeed Frames also bypass the
// drain because the active blocking tool consumes them and surfaces
// the result through its tool_result. The filter only governs Frames
// that landed in pendingInbound via RouteBuffered.

// projectFrameToHistory maps an allow-listed Frame onto the
// model.Message it should fold into s.history. Returns (msg, true)
// for allow-listed kinds and the zero value with false for everything
// else. The mapping is intentionally simple: the model sees every
// surviving Frame as a RoleUser message, with a `[system: <kind>]`
// prefix when the Frame came from the runtime (not the user).
//
// Allow list (phase-4-spec §11):
//   - UserMessage         — adapter input; passes through verbatim.
//   - SubagentStarted     — rendered as [system: spawned_note].
//   - SubagentResult      — rendered as [system: subagent_result];
//                           the result body surfaces verbatim.
//   - SystemMessage       — rendered as [system: <Kind>] <Content>;
//                           runtime-injected nudges (soft_warning,
//                           stuck_nudge, whiteboard, spawned_note).
//
// Everything else falls through. Tool calls / tool results, raw
// reasoning frames, sub-agent's own system_messages, and sub-agent
// plan_op / extension_frame events stay out of the parent's history
// per §11 ("stays in the originating session's events; only
// surfaces via subagent_runs").
func projectFrameToHistory(f protocol.Frame) (model.Message, bool) {
	switch v := f.(type) {
	case *protocol.UserMessage:
		return model.Message{
			Role:    model.RoleUser,
			Content: v.Payload.Text,
		}, true
	case *protocol.SubagentStarted:
		text := fmt.Sprintf("[system: %s] spawned %s (role: %s) at depth %d",
			protocol.SystemMessageSpawnedNote,
			v.Payload.ChildSessionID, v.Payload.Role, v.Payload.Depth)
		return model.Message{Role: model.RoleUser, Content: text}, true
	case *protocol.SubagentResult:
		body := v.Payload.Result
		if body == "" {
			body = fmt.Sprintf("(no result; reason: %s)", v.Payload.Reason)
		}
		text := fmt.Sprintf("[system: subagent_result] %s reason=%s turns=%d\n%s",
			v.Payload.SessionID, v.Payload.Reason, v.Payload.TurnsUsed, body)
		return model.Message{Role: model.RoleUser, Content: text}, true
	case *protocol.SystemMessage:
		text := fmt.Sprintf("[system: %s] %s", v.Payload.Kind, v.Payload.Content)
		return model.Message{Role: model.RoleUser, Content: text}, true
	}
	return model.Message{}, false
}

// visibilityAllows is a thin predicate wrapper over projectFrameToHistory
// for callers that only need to know whether a Frame is allow-listed
// (e.g. tests asserting the filter contract without caring about the
// rendered text). Equivalent to (msg, ok := project; return ok).
func visibilityAllows(f protocol.Frame) bool {
	_, ok := projectFrameToHistory(f)
	return ok
}
