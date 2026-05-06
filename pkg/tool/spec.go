package tool

// Spec is the runtime-side, type-agnostic provider specification —
// the union of fields every concrete ProviderBuilder might consume.
// pkg/tool stays free of pkg/config: callers (pkg/runtime,
// pkg/session) project from config.ToolProviderSpec into Spec at
// the boundary, and Builder dispatches by Spec.Type.
//
// Field set is intentionally permissive — type-specific subpackages
// read only the fields they care about and ignore the rest. Adding
// a new provider type = a new case in providers.Builder + new fields
// here when needed (no breaking change to existing types).
type Spec struct {
	// Name is the registered provider's identifier. Tool names are
	// "<Name>:<tool>"; the provider's Name() method must return
	// this string.
	Name string

	// Type selects the concrete builder. Empty defaults to "mcp"
	// for backwards-compat with the original tool_providers
	// schema. Future values: "webhook", "kubernetes", ...
	Type string

	// Transport is mcp-specific: "stdio" | "streamable-http" |
	// "sse". Other types ignore.
	Transport string

	// Command + Args + Env are the stdio process spawn shape
	// (mcp/stdio). Cwd is the working directory; per-session
	// callers populate it with the session workspace.
	Command string
	Args    []string
	Env     map[string]string
	Cwd     string

	// Endpoint + Headers describe the HTTP wire (mcp/http, sse).
	Endpoint string
	Headers  map[string]string

	// Auth names the auth.Service source whose token / stdio
	// bootstrap should be injected. Empty disables auth handling.
	Auth string

	// Lifetime distinguishes per_agent (root Manager) vs
	// per_session (child Manager) registration.
	Lifetime Lifetime
}
