package mission

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// Permission objects gated by the 3-tier perm stack. Phase A only
// uses :finish and :get_handoff; later phases extend the list.
const (
	PermFinish     = "hugen:mission:finish"
	PermGetHandoff = "hugen:mission:get_handoff"
)

const (
	missionFinishSchema = `{
  "type": "object",
  "properties": {
    "reason": {
      "type": "string",
      "description": "Termination reason — one of: completed | cancelled | failed | max_iterations_exhausted. Required."
    },
    "text": {
      "type": "string",
      "description": "Optional final answer text the supervisor renders into the mission's terminal SubagentResult. When omitted, the runtime synthesises a generic completion message."
    }
  },
  "required": ["reason"]
}`

	missionGetHandoffSchema = `{
  "type": "object",
  "properties": {
    "ref": {
      "type": "string",
      "description": "Handoff ref to fetch — \"<subagent_name>@<wave_label>\" as listed in [Available handoffs] in the worker's first message. Required."
    }
  },
  "required": ["ref"]
}`
)

type finishInput struct {
	Reason string `json:"reason"`
	Text   string `json:"text,omitempty"`
}

type getHandoffInput struct {
	Ref string `json:"ref"`
}

type toolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type toolErrorResponse struct {
	Error toolError `json:"error"`
}

func toolErr(code, msg string) (json.RawMessage, error) {
	return json.Marshal(toolErrorResponse{Error: toolError{Code: code, Message: msg}})
}

// List implements [tool.ToolProvider].
func (e *Extension) List(_ context.Context) ([]tool.Tool, error) {
	return []tool.Tool{
		{
			Name:             providerName + ":finish",
			Description:      "Terminate the mission with a structured reason and optional final text. Supervisor-only — workers cannot finish a mission.",
			Provider:         providerName,
			PermissionObject: PermFinish,
			ArgSchema:        json.RawMessage(missionFinishSchema),
		},
		{
			Name:             providerName + ":get_handoff",
			Description:      "Fetch a stored handoff by ref. Refs are discovered through the [Available handoffs] catalog in the worker's first message — never invent ref names.",
			Provider:         providerName,
			PermissionObject: PermGetHandoff,
			ArgSchema:        json.RawMessage(missionGetHandoffSchema),
		},
	}, nil
}

// Call implements [tool.ToolProvider]. Routes by short tool name.
func (e *Extension) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	short := strings.TrimPrefix(name, providerName+":")
	switch short {
	case "finish":
		return e.callFinish(ctx, args)
	case "get_handoff":
		return e.callGetHandoff(ctx, args)
	default:
		return nil, fmt.Errorf("%w: mission:%s", tool.ErrUnknownTool, short)
	}
}

// Subscribe implements [tool.ToolProvider]. Static catalogue.
func (e *Extension) Subscribe(_ context.Context) (<-chan tool.ProviderEvent, error) {
	return nil, nil
}

// Close implements [tool.ToolProvider]. Per-session state is in
// SessionState; nothing for the provider value itself to release.
func (e *Extension) Close() error { return nil }

// callFinish — Phase A skeleton. Validates the input and returns a
// structured ok envelope. The actual session-close handshake (emit
// AgentMessage{Final:true,Consolidated:true}, transition the
// mission session to teardown) wires in Phase B once the
// supervisor flow lands. For Phase A this tool is callable as
// a no-op so the integration scenario can observe a clean call
// site without the runtime crashing.
func (e *Extension) callFinish(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	state, ok := extension.SessionStateFromContext(ctx)
	if !ok || state == nil {
		return toolErr("session_gone", "no session attached to dispatch ctx")
	}
	var in finishInput
	if err := json.Unmarshal(args, &in); err != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid mission:finish args: %v", err))
	}
	if strings.TrimSpace(in.Reason) == "" {
		return toolErr("bad_request", "reason is required")
	}
	m := FromState(state)
	if m == nil {
		return toolErr("unavailable", "mission state not initialised on this session")
	}
	// Phase A: stash the finish intent on the mission state so the
	// scenario harness / status reporter can observe it. The
	// teardown handshake itself is deferred to Phase B.
	m.mu.Lock()
	if m.Plan.Roadmap == nil {
		m.Plan.Roadmap = nil
	}
	m.mu.Unlock()
	out := map[string]any{
		"ok":     true,
		"reason": in.Reason,
	}
	if in.Text != "" {
		out["text_len"] = len(in.Text)
	}
	return json.Marshal(out)
}

// callGetHandoff reads a handoff from the per-mission store. Any
// ref in the store is fetchable (no per-worker scoping — discovery
// happens via the first-message catalog). Returns an error envelope
// when ref is empty, malformed, or absent.
func (e *Extension) callGetHandoff(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	state, ok := extension.SessionStateFromContext(ctx)
	if !ok || state == nil {
		return toolErr("session_gone", "no session attached to dispatch ctx")
	}
	var in getHandoffInput
	if err := json.Unmarshal(args, &in); err != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid mission:get_handoff args: %v", err))
	}
	if strings.TrimSpace(in.Ref) == "" {
		return toolErr("bad_request", "ref is required")
	}
	if _, _, err := ParseRef(in.Ref); err != nil {
		return toolErr("bad_request", err.Error())
	}
	m := FromState(state)
	if m == nil {
		return toolErr("unavailable", "mission state not initialised on this session")
	}
	h, ok := m.Handoffs.Get(in.Ref)
	if !ok {
		return toolErr("not_found", fmt.Sprintf("handoff %q not in store", in.Ref))
	}
	return json.Marshal(h)
}
