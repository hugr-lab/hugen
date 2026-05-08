package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/tool"
	mcpcli "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// Transport selects the wire protocol a Provider talks. Empty is
// treated as TransportStdio for back-compat.
type Transport string

const (
	TransportStdio          Transport = "stdio"
	TransportStreamableHTTP Transport = "http"
	TransportSSE            Transport = "sse"
)

// Spec is the wire-level provider description. Stdio servers are
// spawned as subprocesses (Command/Args/Env/Cwd); http/sse servers
// connect over HTTP at Endpoint with optional auth applied via a
// shared http.Client RoundTripper.
//
// Spec is the post-projection shape — runtime callers usually start
// from tool.Spec and let New() fill the missing wire-level fields
// (RoundTripper, env additions, etc.). Tests construct Spec
// directly via NewWithSpec.
type Spec struct {
	Name        string            // provider short name (e.g. "bash-mcp", "hugr-main")
	Transport   Transport         // "" → stdio (default); "http" → streamable HTTP; "sse" → SSE
	Command     string            // stdio: executable path
	Args        []string          // stdio: command args
	Env         map[string]string // stdio: child-process env additions
	Cwd         string            // stdio: working directory for the subprocess
	Endpoint    string            // http/sse: base URL
	HTTPClient  *http.Client      // http/sse: optional pre-built client (wins over RoundTripper)
	RoundTripper http.RoundTripper // http/sse: wraps http.DefaultTransport (e.g. auth.Transport(store, base))
	Headers     map[string]string // http/sse: static headers (e.g. X-API-Key); injected on every request
	Lifetime    tool.Lifetime     // honoured for catalogue; spawning/connecting is up to the caller
	PermObject  string            // shared permission_object for every tool ("hugen:tool:bash-mcp")
	Description string            // optional, surfaced as provider description
}

// Provider speaks MCP through mark3labs/mcp-go's Client and
// satisfies tool.ToolProvider so ToolManager can dispatch tool
// calls through it. One instance per registered MCP server.
//
// Recovery contract (phase-4 US7, design-001 §6.7b):
//
//   - List / Call surface upstream errors verbatim. The provider
//     does NOT loop, retry, or classify errors — recovery is the
//     decorator's job (see pkg/tool/providers/recovery).
//   - Provider implements tool.Recoverable. recovery.Wrap consults
//     it on Call/List error: TryReconnect rebuilds the underlying
//     client and the wrapper re-issues the original call.
//   - Close marks the provider as removed; subsequent calls return
//     tool.ErrProviderRemoved. Close is final — TryReconnect on a
//     closed provider returns ErrProviderRemoved.
type Provider struct {
	spec Spec
	log  *slog.Logger

	mu     sync.Mutex
	client *mcpcli.Client
	closed bool

	subsMu sync.Mutex
	subs   []chan tool.ProviderEvent

	// onClose holds teardown callbacks (revoke a minted stdio-auth
	// bootstrap, drop a temp dir, etc.) the provider runs on Close.
	// Phase 4.1c folded the legacy ToolManager.cleanups map into
	// each provider so the registry surface stays free of side
	// state.
	onClose []func()
}

// SetOnClose registers teardown callbacks that fire on Close.
// Idempotent and additive — subsequent calls append. Callers are
// responsible for not registering nil functions; nils are skipped
// at run time but waste a slot.
func (p *Provider) SetOnClose(fns []func()) {
	if len(fns) == 0 {
		return
	}
	p.mu.Lock()
	p.onClose = append(p.onClose, fns...)
	p.mu.Unlock()
}

// NewWithSpec spawns the MCP server, runs the protocol handshake,
// and registers the tools/list_changed notifier. Caller is
// responsible for calling Close on shutdown. Tests use this entry
// point directly; runtime callers go through New(tool.Spec).
func NewWithSpec(ctx context.Context, spec Spec, log *slog.Logger) (*Provider, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if spec.Name == "" {
		return nil, errors.New("mcp: spec missing name")
	}
	switch spec.Transport {
	case "", TransportStdio:
		spec.Transport = TransportStdio
		if spec.Command == "" {
			return nil, errors.New("mcp: stdio spec missing command")
		}
	case TransportStreamableHTTP, TransportSSE:
		if spec.Endpoint == "" {
			return nil, fmt.Errorf("mcp: %s spec missing endpoint", spec.Transport)
		}
	default:
		return nil, fmt.Errorf("mcp: unsupported transport %q (want stdio|http|sse)", spec.Transport)
	}
	p := &Provider{spec: spec, log: log}
	if err := p.connect(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

// newWithClient is the test-only entry point that adopts an
// externally-constructed mcp-go Client (typically the in-process
// variant). It registers the tools/list_changed handler and runs
// Initialize, mirroring what NewWithSpec does after spawning the
// stdio subprocess.
func newWithClient(ctx context.Context, spec Spec, cli *mcpcli.Client, log *slog.Logger) (*Provider, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	p := &Provider{spec: spec, log: log}
	cli.OnNotification(func(n mcp.JSONRPCNotification) {
		if n.Method == mcp.MethodNotificationToolsListChanged {
			p.emit(tool.ProviderEvent{Kind: tool.ProviderToolsChanged})
		}
	})
	if _, err := cli.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo: mcp.Implementation{
				Name:    "hugen",
				Version: "phase-3-test",
			},
		},
	}); err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("mcp: initialize %s: %w", spec.Name, err)
	}
	p.client = cli
	return p, nil
}

func (p *Provider) connect(ctx context.Context) error {
	cli, needsStart, err := p.newClient()
	if err != nil {
		return err
	}
	cli.OnNotification(func(n mcp.JSONRPCNotification) {
		if n.Method == mcp.MethodNotificationToolsListChanged {
			p.emit(tool.ProviderEvent{Kind: tool.ProviderToolsChanged})
		}
	})
	if needsStart {
		if err := cli.Start(ctx); err != nil {
			_ = cli.Close()
			return fmt.Errorf("mcp: start %s: %w", p.spec.Name, err)
		}
	}
	if _, err := cli.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo: mcp.Implementation{
				Name:    "hugen",
				Version: "phase-3",
			},
		},
	}); err != nil {
		_ = cli.Close()
		return fmt.Errorf("mcp: initialize %s: %w", p.spec.Name, err)
	}
	p.mu.Lock()
	p.client = cli
	p.mu.Unlock()
	return nil
}

// newClient builds the underlying mcp-go client for the configured
// transport. The stdio client auto-starts inside its constructor;
// http/sse clients return needsStart=true so the caller can call
// Start before Initialize.
func (p *Provider) newClient() (cli *mcpcli.Client, needsStart bool, err error) {
	switch p.spec.Transport {
	case TransportStdio:
		env := envSlice(p.spec.Env)
		var opts []transport.StdioOption
		if p.spec.Cwd != "" {
			cwd := p.spec.Cwd
			opts = append(opts, transport.WithCommandFunc(func(ctx context.Context, command string, env []string, args []string) (*exec.Cmd, error) {
				c := exec.CommandContext(ctx, command, args...)
				c.Env = env
				c.Dir = cwd
				return c, nil
			}))
		}
		cli, err = mcpcli.NewStdioMCPClientWithOptions(p.spec.Command, env, p.spec.Args, opts...)
		if err != nil {
			return nil, false, fmt.Errorf("mcp: spawn %s: %w", p.spec.Name, err)
		}
		return cli, false, nil

	case TransportStreamableHTTP:
		opts := []transport.StreamableHTTPCOption{}
		if hc := p.httpClient(); hc != nil {
			opts = append(opts, transport.WithHTTPBasicClient(hc))
		}
		if len(p.spec.Headers) > 0 {
			opts = append(opts, transport.WithHTTPHeaders(p.spec.Headers))
		}
		cli, err = mcpcli.NewStreamableHttpClient(p.spec.Endpoint, opts...)
		if err != nil {
			return nil, false, fmt.Errorf("mcp: connect %s: %w", p.spec.Name, err)
		}
		return cli, true, nil

	case TransportSSE:
		opts := []transport.ClientOption{}
		if hc := p.httpClient(); hc != nil {
			opts = append(opts, transport.WithHTTPClient(hc))
		}
		if len(p.spec.Headers) > 0 {
			opts = append(opts, transport.WithHeaders(p.spec.Headers))
		}
		cli, err = mcpcli.NewSSEMCPClient(p.spec.Endpoint, opts...)
		if err != nil {
			return nil, false, fmt.Errorf("mcp: connect %s: %w", p.spec.Name, err)
		}
		return cli, true, nil

	default:
		return nil, false, fmt.Errorf("mcp: unsupported transport %q", p.spec.Transport)
	}
}

// httpClient resolves the *http.Client passed to mark3labs's HTTP
// transports. Order: explicit HTTPClient > RoundTripper-wrapped
// client > nil (mark3labs uses its default).
func (p *Provider) httpClient() *http.Client {
	if p.spec.HTTPClient != nil {
		return p.spec.HTTPClient
	}
	if p.spec.RoundTripper != nil {
		return &http.Client{Transport: p.spec.RoundTripper}
	}
	return nil
}

func (p *Provider) currentClient() (*mcpcli.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, tool.ErrProviderRemoved
	}
	if p.client == nil {
		return nil, fmt.Errorf("mcp: %s not connected", p.spec.Name)
	}
	return p.client, nil
}

// TryReconnect rebuilds the underlying mcp-go client and re-runs
// Initialize. Implements tool.Recoverable so recovery.Wrap can
// drive the retry loop.
//
// Closed providers cannot be reconnected — TryReconnect returns
// tool.ErrProviderRemoved without attempting anything. On any
// other state TryReconnect drops the old client (close errors
// ignored — the connection is presumed dead) and dials a fresh
// one. Returns nil on success; the original error otherwise.
func (p *Provider) TryReconnect(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return tool.ErrProviderRemoved
	}
	if p.client != nil {
		_ = p.client.Close()
		p.client = nil
	}
	p.mu.Unlock()
	if err := p.connect(ctx); err != nil {
		return err
	}
	p.emit(tool.ProviderEvent{Kind: tool.ProviderHealthChanged, Data: tool.HealthHealthy})
	return nil
}

func (p *Provider) Name() string            { return p.spec.Name }
func (p *Provider) Lifetime() tool.Lifetime { return p.spec.Lifetime }

func (p *Provider) List(ctx context.Context) ([]tool.Tool, error) {
	cli, err := p.currentClient()
	if err != nil {
		return nil, err
	}
	res, err := cli.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return nil, fmt.Errorf("mcp: list %s: %w", p.spec.Name, err)
	}
	out := make([]tool.Tool, 0, len(res.Tools))
	for _, t := range res.Tools {
		schema, _ := json.Marshal(t.InputSchema)
		fqName := p.spec.Name + ":" + t.Name
		// Vendored MCP tools (motherduck, etc.) routinely emit
		// `additionalProperties` or arrays without `items` — that
		// fails downstream at the chat-completion provider.
		// Sanitise in-place; if anything changed, log once so
		// operators can see it.
		cleaned, notes, err := tool.SanitizeLLMSchema(schema)
		if err != nil {
			p.log.Warn("mcp: invalid schema, dropping tool",
				"provider", p.spec.Name, "tool", fqName, "err", err)
			continue
		}
		if len(notes) > 0 {
			p.log.Warn("mcp: schema sanitised",
				"provider", p.spec.Name, "tool", fqName, "repairs", notes)
		}
		out = append(out, tool.Tool{
			Name:             fqName,
			Description:      t.Description,
			ArgSchema:        cleaned,
			Provider:         p.spec.Name,
			PermissionObject: p.spec.PermObject,
		})
	}
	return out, nil
}

// Call dispatches a tool call. Args are passed verbatim; the
// caller (ToolManager) has already merged Tier-1/2 Data into
// effective args. Errors surface verbatim — the recovery decorator
// (pkg/tool/providers/recovery) drives any retry loop.
func (p *Provider) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	cli, err := p.currentClient()
	if err != nil {
		return nil, err
	}
	var argsAny any
	if len(args) == 0 {
		argsAny = map[string]any{}
	} else {
		argsAny = json.RawMessage(args)
	}
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = argsAny
	// Per_agent MCPs (hugr-query today) need the session id to
	// route file output under the right per-session workspace.
	// Per_session MCPs see the same field but ignore it. Tests
	// that bypass perm.WithSession leave Meta nil, which both
	// kinds tolerate.
	if sc, ok := perm.SessionFromContext(ctx); ok && sc.SessionID != "" {
		req.Params.Meta = &mcp.Meta{
			AdditionalFields: map[string]any{"session_id": sc.SessionID},
		}
	}

	res, err := cli.CallTool(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("mcp: call %s.%s: %w", p.spec.Name, name, err)
	}
	return marshalCallResult(res)
}

func marshalCallResult(res *mcp.CallToolResult) (json.RawMessage, error) {
	if res == nil {
		return json.RawMessage(`null`), nil
	}
	if res.StructuredContent != nil {
		return json.Marshal(res.StructuredContent)
	}
	// Concatenate text contents; skip non-text variants for now.
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	if res.IsError {
		return json.Marshal(map[string]any{"is_error": true, "text": sb.String()})
	}
	return json.Marshal(map[string]any{"text": sb.String()})
}

func (p *Provider) Subscribe(ctx context.Context) (<-chan tool.ProviderEvent, error) {
	ch := make(chan tool.ProviderEvent, 8)
	p.subsMu.Lock()
	p.subs = append(p.subs, ch)
	p.subsMu.Unlock()
	go func() {
		<-ctx.Done()
		p.subsMu.Lock()
		for i, c := range p.subs {
			if c == ch {
				p.subs = append(p.subs[:i], p.subs[i+1:]...)
				break
			}
		}
		p.subsMu.Unlock()
		close(ch)
	}()
	return ch, nil
}

func (p *Provider) emit(ev tool.ProviderEvent) {
	p.subsMu.Lock()
	defer p.subsMu.Unlock()
	for _, ch := range p.subs {
		select {
		case ch <- ev:
		default:
			// Drop on slow consumer; the provider keeps making progress.
		}
	}
}

// Close terminates the underlying client and marks the provider
// as removed. Idempotent.
func (p *Provider) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	cli := p.client
	p.client = nil
	teardown := p.onClose
	p.onClose = nil
	p.mu.Unlock()

	var err error
	if cli != nil {
		err = cli.Close()
	}
	for _, fn := range teardown {
		if fn != nil {
			fn()
		}
	}
	return err
}

// envSlice produces the env slice handed to a stdio MCP child.
// The child inherits hugen's own os.Environ (PATH, locale, HOME,
// etc.) so common shell binaries (sh, bash, du, ls) and language
// runtimes resolve at exec time. The configured spec.Env overrides
// any inherited keys with the same name and contributes new ones.
// Without the inheritance step children would launch with
// PATH="" — every shell command in bash-mcp would fail "executable
// not found".
func envSlice(env map[string]string) []string {
	parent := os.Environ()
	if len(env) == 0 {
		return parent
	}
	indexByKey := make(map[string]int, len(parent))
	for i, kv := range parent {
		if eq := strings.IndexByte(kv, '='); eq > 0 {
			indexByKey[kv[:eq]] = i
		}
	}
	out := append([]string(nil), parent...)
	for k, v := range env {
		entry := k + "=" + v
		if idx, ok := indexByKey[k]; ok {
			out[idx] = entry
		} else {
			out = append(out, entry)
		}
	}
	return out
}

// ensure Provider satisfies the contracts declared in pkg/tool.
var (
	_ tool.ToolProvider = (*Provider)(nil)
	_ tool.Recoverable  = (*Provider)(nil)
)

