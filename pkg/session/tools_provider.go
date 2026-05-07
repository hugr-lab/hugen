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
// funcs in tools_subagent.go / tools_plan.go / … — handlers are
// methods on *Session so they read s.store / s.logger / s.perms /
// s.skills directly without an injected leaf-deps host.
//
// Lifetime is PerSession so Resources.Acquire registers each
// Session onto its own child ToolManager; teardown drops the
// registration when the child Closes on Release.
//
// The WithSession / SessionFromContext helpers stay
// (pkg/session/context.go) as the escape-hatch for any future
// third-party session-aware provider that registers on root and
// lacks a direct *Session field.

const sessionToolProviderName = "session"

func (s *Session) initTools() {
	s.sessionTools = map[string]sessionToolDescriptor{}
	s.initSubagent()
	s.initPlan()
	s.initWhiteboard()
}

// sessionToolHandler dispatches one session-scoped tool call.
// First arg is the calling session; the package-level dispatch
// table holds method values like (*Session).callSpawnSubagent.
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

// toolError is the JSON shape every session-scoped tool returns on
// a model-visible error. The LLM sees
// {"error":{"code":..., "message":...}} so it can react without
// conflating the failure with infrastructure errors emitted via
// tool_error frames.
type toolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
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
