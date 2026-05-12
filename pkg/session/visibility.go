package session

import (
	"fmt"
	"strings"

	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/prompts"
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
// extension_frame events stay out of the parent's history
// per §11 ("stays in the originating session's events; only
// surfaces via subagent_runs").
func projectFrameToHistory(r *prompts.Renderer, f protocol.Frame) (model.Message, bool) {
	switch v := f.(type) {
	case *protocol.UserMessage:
		return model.Message{
			Role:    model.RoleUser,
			Content: v.Payload.Text,
		}, true
	case *protocol.SubagentStarted:
		body := strings.TrimRight(r.MustRender(
			"system/spawned_note",
			map[string]any{
				"ChildID": v.Payload.ChildSessionID,
				"Role":    v.Payload.Role,
				"Depth":   v.Payload.Depth,
			},
		), "\n")
		text := fmt.Sprintf("[system: %s] %s",
			protocol.SystemMessageSpawnedNote, body)
		return model.Message{Role: model.RoleUser, Content: text}, true
	case *protocol.SubagentResult:
		resBody := v.Payload.Result
		if resBody == "" {
			resBody = fmt.Sprintf("(no result; reason: %s)", v.Payload.Reason)
		}
		body := strings.TrimRight(r.MustRender(
			"system/subagent_result_render",
			map[string]any{
				"ChildID": v.Payload.SessionID,
				"Reason":  v.Payload.Reason,
				"Turns":   v.Payload.TurnsUsed,
				"Body":    resBody,
			},
		), "\n")
		text := "[system: subagent_result] " + body
		return model.Message{Role: model.RoleUser, Content: text}, true
	case *protocol.SystemMessage:
		text := fmt.Sprintf("[system: %s] %s", v.Payload.Kind, v.Payload.Content)
		return model.Message{Role: model.RoleUser, Content: text}, true
	}
	return model.Message{}, false
}

// visibilityAllows is a pure-predicate version of the §11 allow-list
// for callers (e.g. tests asserting the filter contract) that only
// need to know whether a Frame is model-visible without rendering
// it. Keep the cases in lockstep with projectFrameToHistory.
func visibilityAllows(f protocol.Frame) bool {
	switch f.(type) {
	case *protocol.UserMessage,
		*protocol.SubagentStarted,
		*protocol.SubagentResult,
		*protocol.SystemMessage:
		return true
	}
	return false
}
