package tool

import (
	"context"
	"encoding/json"
	"errors"
)

// Tool is the leaf — what the LLM dispatches.
//
// Name is fully-qualified: "<provider>:<tool_name>" (e.g.
// "bash-mcp:bash.write_file"). The provider's Name() must equal
// the prefix; ToolManager enforces this when adding providers.
type Tool struct {
	Name             string          `json:"name"`
	Description      string          `json:"description"`
	ArgSchema        json.RawMessage `json:"arg_schema,omitempty"`
	Provider         string          `json:"provider"`
	PermissionObject string          `json:"permission_object"`
}

// Lifetime tags how a provider relates to agent/session
// boundaries. Phase-3 ships per_agent for bash-mcp / hugr-query;
// per_session lifetime arrives in phase-3.5 with the stateful
// MCPs (duckdb-mcp, python-mcp).
type Lifetime int

const (
	LifetimePerAgent Lifetime = iota
	LifetimePerSession
	LifetimeExternal
)

func (l Lifetime) String() string {
	switch l {
	case LifetimePerAgent:
		return "per_agent"
	case LifetimePerSession:
		return "per_session"
	case LifetimeExternal:
		return "external"
	default:
		return "unknown"
	}
}

// ToolProvider exposes a group of tools and dispatches calls.
//
// Implementations live alongside this package: SystemProvider
// (built-in), MCPProvider (mark3labs/mcp-go client). Other
// implementations satisfy the contract structurally.
type ToolProvider interface {
	// Name returns the provider's short name; matches the prefix
	// of every Tool the provider exposes (e.g. "bash-mcp").
	Name() string

	// Lifetime is informational; the runtime uses it to decide
	// when to (re)spawn the provider's underlying process.
	Lifetime() Lifetime

	// List returns every Tool the provider currently exposes.
	// The catalogue may change over a provider's lifetime; the
	// provider signals via Subscribe/ProviderToolsChanged.
	List(ctx context.Context) ([]Tool, error)

	// Call dispatches a single tool call. The args have already
	// been gated through PermissionService.Resolve and run
	// through template.Apply by the time Call is invoked. Result
	// is opaque JSON; an error from Call surfaces as a tool_error
	// frame upstream.
	Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error)

	// Subscribe streams ProviderEvent notifications. Providers
	// without dynamic tool changes return nil, nil.
	Subscribe(ctx context.Context) (<-chan ProviderEvent, error)

	// Close releases the provider's resources (close the MCP
	// client, terminate the spawned subprocess, etc.). Idempotent.
	Close() error
}

// ProviderEvent is what Subscribe streams.
type ProviderEvent struct {
	Kind ProviderEventKind
	// Data is event-specific. For ProviderToolsChanged it may
	// carry the new []Tool snapshot; for ProviderHealthChanged
	// it carries a HealthStatus; for ProviderTerminated it
	// carries the underlying error (or nil on clean shutdown).
	Data any
}

type ProviderEventKind int

const (
	ProviderToolsChanged ProviderEventKind = iota // notifications/tools/list_changed
	ProviderHealthChanged
	ProviderTerminated
)

// HealthStatus is the value carried in ProviderHealthChanged.Data.
type HealthStatus int

const (
	HealthUnknown HealthStatus = iota
	HealthHealthy
	HealthDegraded
	HealthDead
)

// Errors. Sentinel values, errors.Is-comparable.
var (
	ErrUnknownTool      = errors.New("tool: unknown")
	ErrUnknownProvider  = errors.New("tool: unknown provider")
	ErrPermissionDenied = errors.New("tool: permission denied")
	ErrProviderRemoved  = errors.New("tool: provider removed mid-call")
	ErrSnapshotStale    = errors.New("tool: snapshot stale (rebuild needed)")
	ErrArgValidation    = errors.New("tool: args failed schema validation")
)

// Identity is what the manager passes to PermissionService and
// the policy store. Mirrors pkg/auth/perm.Identity but stays in
// this package to avoid the cycle (pkg/tool already imports
// pkg/auth/perm).
type Identity struct {
	UserID          string
	AgentID         string
	Role            string
	Roles           []string
	SessionID       string
	SessionMetadata map[string]string
}
