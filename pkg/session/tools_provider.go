package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hugr-lab/hugen/pkg/tool"
)

// Session implements tool.ToolProvider directly. The "session:*"
// catalogue is a static dispatch table populated by per-tool init()
// funcs in tools_subagent.go — handlers are methods on *Session so
// they read s.store / s.logger / s.perms directly without an
// injected leaf-deps host.
//
// Lifetime is PerSession so the per-session ToolManager registers
// each Session onto its own child manager; teardown drops the
// registration when the child Closes on Release.

const sessionToolProviderName = "session"

func (s *Session) initTools() {
	s.sessionTools = map[string]sessionToolDescriptor{}
	s.initSubagent()
}

// sessionToolHandler dispatches one session-scoped tool call. The
// dispatch table holds method values like (*Session).callSpawnSubagent.
type sessionToolHandler func(ctx context.Context, args json.RawMessage) (json.RawMessage, error)

// sessionToolDescriptor is the runtime metadata used to project a
// registered tool into the tool.Tool catalogue. The schema is
// JSON-Schema-shaped (raw bytes so the LLM-provider layer passes
// it through verbatim). PermissionObject is the Tier-1 key the
// permission stack uses to gate the dispatch.
type sessionToolDescriptor struct {
	Name             string
	Description      string
	ArgSchema        json.RawMessage
	PermissionObject string
	Handler          sessionToolHandler
}

// Name implements tool.ToolProvider.
func (s *Session) Name() string { return sessionToolProviderName }

// Lifetime implements tool.ToolProvider. One provider per live
// session — registered onto the session's child ToolManager during
// Resources.Acquire and removed when the child Closes on Release.
func (s *Session) Lifetime() tool.Lifetime { return tool.LifetimePerSession }

// List implements tool.ToolProvider. Projects the static
// sessionTools table to []tool.Tool with the canonical
// "session:<name>" prefix the rest of ToolManager expects.
func (s *Session) List(_ context.Context) ([]tool.Tool, error) {
	if len(s.sessionTools) == 0 {
		return nil, nil
	}
	out := make([]tool.Tool, 0, len(s.sessionTools))
	for _, d := range s.sessionTools {
		out = append(out, tool.Tool{
			Name:             sessionToolProviderName + ":" + d.Name,
			Description:      d.Description,
			ArgSchema:        d.ArgSchema,
			Provider:         sessionToolProviderName,
			PermissionObject: d.PermissionObject,
		})
	}
	return out, nil
}

// Call implements tool.ToolProvider. Strips the "session:" prefix,
// looks up the handler in sessionTools, and invokes it as a method
// on this Session.
func (s *Session) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	short := strings.TrimPrefix(name, sessionToolProviderName+":")
	d, ok := s.sessionTools[short]
	if !ok {
		return nil, fmt.Errorf("%w: session:%s", tool.ErrUnknownTool, short)
	}
	return d.Handler(ctx, args)
}

// Subscribe implements tool.ToolProvider. The session catalogue is
// static (sessionTools is package-level + immutable after init) so
// there's nothing to subscribe to.
func (s *Session) Subscribe(_ context.Context) (<-chan tool.ProviderEvent, error) {
	return nil, nil
}

// Close implements tool.ToolProvider. The provider holds no
// resources of its own — *Session lifecycle is owned by Manager +
// Resources, so this is a no-op. ToolManager calls Close on
// providers it deregisters, hence the explicit nil return.
func (s *Session) Close() error { return nil }

// composeChildFirstMessage builds the worker's first user message
// from the parent's task string, an optional inputs blob, and an
// optional pre-rendered whiteboard digest. Non-empty blocks land
// at the top of the message in this order:
//
//	[Whiteboard]                       ← shared accumulated findings
//	<digest, see whiteboard.FormatForHandoff>
//
//	[Inputs from parent]               ← mission-supplied facts for THIS step
//	{
//	  "module": "op2023",
//	  ...
//	}
//
//	[Task]                             ← what to do now
//	<original task>
//
// Read top-down: whiteboard = what siblings already produced,
// inputs = mission's specific brief for this worker, task = the
// instruction. Together they kill the two duplication patterns we
// saw on weak models: planner→worker re-discovery (5.4.c.4 — the
// inputs block) and cross-wave re-introspection (5.4.c.9 — the
// whiteboard block).
//
// Returns task verbatim when both inputs and whiteboard are empty
// / trivial — the spawn surface degrades gracefully when callers
// don't pass them.
func composeChildFirstMessage(task string, inputs any, whiteboard string) string {
	var parts []string
	if wb := strings.TrimSpace(whiteboard); wb != "" {
		parts = append(parts, "[Whiteboard]\n"+wb)
	}
	if inputs != nil {
		body, err := json.MarshalIndent(inputs, "", "  ")
		if err == nil {
			trimmed := strings.TrimSpace(string(body))
			switch trimmed {
			case "", "null", "{}", "[]", `""`:
				// trivial — skip
			default:
				parts = append(parts, "[Inputs from parent]\n"+trimmed)
			}
		}
	}
	if len(parts) == 0 {
		return task
	}
	parts = append(parts, "[Task]\n"+task)
	return strings.Join(parts, "\n\n")
}

// toolError is the JSON shape every session-scoped tool returns on
// a model-visible error. The LLM sees
// {"error":{"code":..., "message":...}} so it can react without
// conflating the failure with infrastructure errors emitted via
// tool_error frames.
//
// Got + ExpectedShape are the optional self-correction hint:
// validation failures that know what the caller sent and what they
// should have sent fill these so weak models see the example
// directly in tool_result. Omitting them keeps the envelope
// backward-compatible for tools that don't track shape (most
// runtime errors).
type toolError struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	Got           any    `json:"got,omitempty"`
	ExpectedShape any    `json:"expected_shape,omitempty"`
}

type toolErrorResponse struct {
	Error toolError `json:"error"`
}

func toolErr(code, msg string) (json.RawMessage, error) {
	resp, err := json.Marshal(toolErrorResponse{Error: toolError{Code: code, Message: msg}})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// toolErrShape is the same as toolErr but carries a self-correction
// hint: the args the caller actually sent (got) and a concrete
// example of the shape the tool expected. Weak models (gemma4-26b
// and similar) drift into "spawn_wave({})" or "spawn_subagent({})"
// loops because the bare "subagents must be a non-empty array"
// message in their last tool_result doesn't show what *would*
// work. Embedding a shape example breaks that loop on the next
// turn without an extra round-trip through the catalogue.
//
// `got` is encoded verbatim — typically the parsed args map; pass
// the raw json.RawMessage when args don't unmarshal cleanly.
// `expected` is a Go literal whose JSON encoding represents one
// valid call (placeholders inside string fields are fine, the
// model uses it as a template).
func toolErrShape(code, msg string, got, expected any) (json.RawMessage, error) {
	resp, err := json.Marshal(toolErrorResponse{Error: toolError{
		Code:          code,
		Message:       msg,
		Got:           got,
		ExpectedShape: expected,
	}})
	if err != nil {
		return nil, err
	}
	return resp, nil
}
