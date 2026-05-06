package admin

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/tool"
)

// providerName matches the Provider field of every Tool exposed
// by AdminProvider — so tool names render as "tool:<field>".
const providerName = "tool"

// AdminProvider exposes a thin, type-agnostic registry surface
// over its ToolManager. It is constructed once in pkg/runtime
// and registered with the agent-level (root) ToolManager so the
// LLM can mutate the per_agent provider set at runtime.
//
// AdminProvider does NOT cover per_session providers — those are
// owned by the session's child Manager. Per-session reload lives
// on the session lifecycle, not under this provider's tools.
type AdminProvider struct {
	tools *tool.ToolManager
}

// New constructs an AdminProvider bound to the supplied Manager.
// Panics on a nil Manager — there is no useful disabled mode for
// a registry-mutation provider.
func New(tools *tool.ToolManager) *AdminProvider {
	if tools == nil {
		panic("admin: nil ToolManager")
	}
	return &AdminProvider{tools: tools}
}

// Name implements tool.ToolProvider. Matches the prefix of every
// Tool the provider exposes ("tool:provider_add", "tool:provider_remove").
func (a *AdminProvider) Name() string { return providerName }

// Lifetime classifies the provider as agent-scoped — one instance
// per agent persists across sessions.
func (a *AdminProvider) Lifetime() tool.Lifetime { return tool.LifetimePerAgent }

// Subscribe is a no-op — AdminProvider has no event stream.
func (a *AdminProvider) Subscribe(ctx context.Context) (<-chan tool.ProviderEvent, error) {
	return nil, nil
}

// Close is a no-op — AdminProvider holds no resources of its own.
func (a *AdminProvider) Close() error { return nil }

// List returns the registry-administration tools.
func (a *AdminProvider) List(ctx context.Context) ([]tool.Tool, error) {
	return []tool.Tool{
		{
			Name:             "tool:provider_add",
			Description:      "Register a new tool provider at runtime. The Spec.Type field selects the concrete implementation (today: \"mcp\"). The new provider is dispatched through the runtime-wired ProviderBuilder; failures from Build (bad spec, unreachable endpoint, auth missing) surface as a tool error.",
			Provider:         providerName,
			PermissionObject: "hugen:tool:provider_add",
			ArgSchema:        schemaAdd,
		},
		{
			Name:             "tool:provider_remove",
			Description:      "Drain and dispose a registered tool provider by name. Idempotent — removing a missing provider returns ErrUnknownProvider.",
			Provider:         providerName,
			PermissionObject: "hugen:tool:provider_remove",
			ArgSchema:        schemaRemove,
		},
	}, nil
}

// Call dispatches the two registry-admin tools. Args have already
// been validated by ToolManager.Resolve before reaching here.
func (a *AdminProvider) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	switch name {
	case "provider_add":
		return a.callAdd(ctx, args)
	case "provider_remove":
		return a.callRemove(ctx, args)
	default:
		return nil, fmt.Errorf("%w: %s", tool.ErrUnknownTool, name)
	}
}

func (a *AdminProvider) callAdd(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	var spec tool.Spec
	if err := json.Unmarshal(args, &spec); err != nil {
		return nil, fmt.Errorf("%w: tool:provider_add: %v", tool.ErrArgValidation, err)
	}
	if spec.Name == "" {
		return nil, fmt.Errorf("%w: tool:provider_add: name required", tool.ErrArgValidation)
	}
	if err := a.tools.AddBySpec(ctx, spec); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"added": spec.Name, "type": spec.Type})
}

func (a *AdminProvider) callRemove(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, fmt.Errorf("%w: tool:provider_remove: %v", tool.ErrArgValidation, err)
	}
	if req.Name == "" {
		return nil, fmt.Errorf("%w: tool:provider_remove: name required", tool.ErrArgValidation)
	}
	if err := a.tools.RemoveProvider(ctx, req.Name); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"removed": req.Name})
}

var (
	schemaAdd = json.RawMessage(`{
  "type": "object",
  "properties": {
    "name":      {"type": "string", "description": "Provider short name. Must be unique within the agent."},
    "type":      {"type": "string", "description": "Provider type. Empty defaults to \"mcp\"."},
    "transport": {"type": "string", "description": "MCP transport: \"stdio\" | \"streamable-http\" | \"sse\". Ignored by non-MCP types."},
    "command":   {"type": "string", "description": "Stdio: executable path."},
    "args":      {"type": "array",  "items": {"type": "string"}, "description": "Stdio: command-line arguments."},
    "env":       {"type": "object", "description": "Stdio: extra env vars merged into the spawn."},
    "endpoint":  {"type": "string", "description": "HTTP/SSE: base URL."},
    "headers":   {"type": "object", "description": "HTTP/SSE: static headers injected on every request."},
    "auth":      {"type": "string", "description": "Optional auth source name (resolved through auth.Service)."}
  },
  "required": ["name"]
}`)
	schemaRemove = json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "Registered provider name."}
  },
  "required": ["name"]
}`)
)

// ensure AdminProvider satisfies tool.ToolProvider.
var _ tool.ToolProvider = (*AdminProvider)(nil)
