package stuckdetector

import (
	"context"
	"encoding/json"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// OnFrameEmit implements [extension.FrameObserver]. Two kinds
// matter for the four detectors:
//
//   - AgentMessage{Consolidated, ToolCalls!=nil}: the model just
//     dispatched a batch of tool calls. Sample each into
//     recentHashes (errCode empty until the matching tool_result
//     arrives). The repeated_hash + tight_density detectors fire
//     here when N identical hashes sit at the tail.
//   - ToolResult: annotate the matching sample by tool_id with
//     the error code (empty when IsError=false). The
//     repeated_error + no_progress detectors fire here when the
//     latest sample's errCode plus the window match.
//
// Other frame kinds are no-ops. The session emits soft warnings
// + hard ceilings from `pkg/session/turn_ceiling.go` — those
// stay on the turn-loop side because they depend on the
// iter / cap pair stored in `turnState`.
func (e *Extension) OnFrameEmit(ctx context.Context, state extension.SessionState, frame protocol.Frame) {
	s := FromState(state)
	if s == nil {
		return
	}
	switch f := frame.(type) {
	case *protocol.AgentMessage:
		if !f.Payload.Consolidated || len(f.Payload.ToolCalls) == 0 {
			return
		}
		at := frame.OccurredAt()
		for _, tc := range f.Payload.ToolCalls {
			s.recordSample(hashSample{
				hash:   LocalToolHash(tc.Name, tc.Args),
				tool:   tc.Name,
				toolID: tc.ToolID,
				at:     at,
			})
		}
		e.evaluate(ctx, state, s)
	case *protocol.ToolResult:
		errCode := ""
		if f.Payload.IsError {
			errCode = errorCodeFromResult(f.Payload.Result)
		}
		if !s.annotateResult(f.Payload.ToolID, errCode) {
			// No matching call sample (rare: spurious result /
			// session boundary). Nothing to evaluate.
			return
		}
		e.evaluate(ctx, state, s)
	}
}

// errorCodeFromResult lifts `error.code` out of a ToolResult
// frame's `result` field, falling back to "unknown" for shapes
// that don't parse. Handles the three concrete cases:
//
//   - protocol.ToolError struct — provider built it via the
//     `protocol.ToolError{...}` ctor.
//   - JSON-bytes (json.RawMessage / []byte / string) — already
//     marshalled.
//   - map[string]any with an "error":{"code":…} shape — generic.
//
// Everything else falls through to parseToolErrorCode([]byte) =
// "unknown" so the repeated_error detector still has a cluster
// key.
func errorCodeFromResult(v any) string {
	switch t := v.(type) {
	case nil:
		return "unknown"
	case protocol.ToolError:
		if t.Code != "" {
			return t.Code
		}
		return "unknown"
	case *protocol.ToolError:
		if t == nil || t.Code == "" {
			return "unknown"
		}
		return t.Code
	case string:
		return parseToolErrorCode([]byte(t))
	case []byte:
		return parseToolErrorCode(t)
	case json.RawMessage:
		return parseToolErrorCode(t)
	case map[string]any:
		if errObj, ok := t["error"].(map[string]any); ok {
			if code, _ := errObj["code"].(string); code != "" {
				return code
			}
		}
	}
	// Last resort: serialise and re-parse. The ToolResultPayload
	// JSON-encodes the value on the wire, so this round-trip
	// matches what an adapter would see.
	b, err := json.Marshal(v)
	if err != nil {
		return "unknown"
	}
	return parseToolErrorCode(b)
}
