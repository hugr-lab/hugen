package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// SessionToolHost bundles the leaf dependencies session-scoped tool
// handlers need beyond the *Session itself: the persistence store
// (transcript reads / writes outside the live session), the logger
// shared with the rest of the runtime, and the permission service
// tools like skill_files consult.
//
// pkg/runtime constructs a single SessionToolHost at boot and
// threads it through ResourceDeps.SessionTools so every Session's
// child ToolManager registers a SessionToolProvider closing over
// the same leaf bag.
type SessionToolHost struct {
	Store  RuntimeStore
	Logger *slog.Logger
	Perms  perm.Service
}

// SessionToolProvider is the per-session ToolProvider that exposes
// session:* tools to the LLM. Lifetime=PerSession — Resources.Acquire
// instantiates one per live session and registers it onto the
// session's child ToolManager so dispatch routes to handlers that
// already hold the calling *Session.
//
// Replaces the phase-4.1a "Manager-as-ToolProvider" pattern: handlers
// no longer recover the *Session via SessionFromContext. The
// WithSession / SessionFromContext helpers stay (pkg/session/context.go)
// as the escape-hatch for any future third-party session-aware
// provider that registers on root and lacks a direct *Session field.
type SessionToolProvider struct {
	s    *Session
	host SessionToolHost
}

// NewSessionToolProvider builds a per-session provider for s closing
// over host. Caller (Resources.Acquire) registers the returned
// provider onto the session's child ToolManager.
func NewSessionToolProvider(s *Session, host SessionToolHost) *SessionToolProvider {
	return &SessionToolProvider{s: s, host: host}
}

const sessionToolProviderName = "session"

// sessionToolHandler dispatches one session-scoped tool call. Receives
// *Session + SessionToolHost directly — no ctx-stash recovery needed.
type sessionToolHandler func(ctx context.Context, s *Session, host SessionToolHost, args json.RawMessage) (json.RawMessage, error)

// sessionToolDescriptor is the runtime metadata used to project a
// registered tool into the tool.Tool catalogue. The schema is
// JSON-Schema-shaped (raw bytes so the LLM-provider layer passes it
// through verbatim). PermissionObject is the Tier-1 key the
// permission stack uses to gate the dispatch.
type sessionToolDescriptor struct {
	Name             string
	Description      string
	ArgSchema        json.RawMessage
	PermissionObject string
	Handler          sessionToolHandler
}

// sessionTools is the static dispatch table. Per-tool init() funcs in
// tools_subagent.go / tools_plan.go / … register their entries at
// package-init time; the table is read-only thereafter so dispatch
// needs no lock.
var sessionTools = map[string]sessionToolDescriptor{}

// Name implements tool.ToolProvider.
func (p *SessionToolProvider) Name() string { return sessionToolProviderName }

// Lifetime implements tool.ToolProvider. One provider per live
// session — registered onto the session's child ToolManager during
// Resources.Acquire and removed when the child Closes on Release.
func (p *SessionToolProvider) Lifetime() tool.Lifetime { return tool.LifetimePerSession }

// List implements tool.ToolProvider. Projects the static
// sessionTools table to []tool.Tool with the canonical
// "session:<name>" prefix the rest of ToolManager expects.
func (p *SessionToolProvider) List(_ context.Context) ([]tool.Tool, error) {
	if len(sessionTools) == 0 {
		return nil, nil
	}
	out := make([]tool.Tool, 0, len(sessionTools))
	for _, d := range sessionTools {
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
// looks up the handler in sessionTools, and invokes it with the
// per-session *Session + SessionToolHost closed over at construction.
func (p *SessionToolProvider) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	short := name
	prefix := sessionToolProviderName + ":"
	if len(name) > len(prefix) && name[:len(prefix)] == prefix {
		short = name[len(prefix):]
	}
	d, ok := sessionTools[short]
	if !ok {
		return nil, fmt.Errorf("%w: session:%s", tool.ErrUnknownTool, short)
	}
	return d.Handler(ctx, p.s, p.host, args)
}

// Subscribe implements tool.ToolProvider. The session catalogue is
// static (sessionTools is package-level + immutable after init) so
// there's nothing to subscribe to.
func (p *SessionToolProvider) Subscribe(_ context.Context) (<-chan tool.ProviderEvent, error) {
	return nil, nil
}

// Close implements tool.ToolProvider. The provider holds no resources
// of its own — *Session lifecycle is owned by Manager + Resources.
func (p *SessionToolProvider) Close() error { return nil }

// toolError is the JSON shape every session-scoped tool returns on a
// model-visible error. The LLM sees {"error":{"code":..., "message":...}}
// so it can react without conflating the failure with infrastructure
// errors emitted via tool_error frames.
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
