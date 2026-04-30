package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"

	mcpcli "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// MCPTransport selects the wire protocol an MCPProvider talks. Empty
// is treated as TransportStdio for back-compat.
type MCPTransport string

const (
	TransportStdio          MCPTransport = "stdio"
	TransportStreamableHTTP MCPTransport = "http"
	TransportSSE            MCPTransport = "sse"
)

// MCPProviderSpec describes how to reach an MCP server. Stdio servers
// are spawned as subprocesses (Command/Args/Env/Cwd); http/sse servers
// are connected over HTTP at Endpoint with optional auth applied via a
// shared http.Client RoundTripper.
type MCPProviderSpec struct {
	Name        string            // provider short name (e.g. "bash-mcp", "hugr-main")
	Transport   MCPTransport      // "" → stdio (default); "http" → streamable HTTP; "sse" → SSE
	Command     string            // stdio: executable path
	Args        []string          // stdio: command args
	Env         map[string]string // stdio: child-process env additions
	Cwd         string            // stdio: working directory for the subprocess
	Endpoint    string            // http/sse: base URL
	HTTPClient  *http.Client      // http/sse: optional pre-built client (wins over RoundTripper)
	RoundTripper http.RoundTripper // http/sse: wraps http.DefaultTransport (e.g. auth.Transport(store, base))
	Headers     map[string]string // http/sse: static headers (e.g. X-API-Key); injected on every request
	Lifetime    Lifetime          // honoured for catalogue; spawning/connecting is up to the caller
	PermObject  string            // shared permission_object for every tool ("hugen:tool:bash-mcp")
	Description string            // optional, surfaced as provider description
}

// MCPProvider wraps mark3labs/mcp-go's stdio Client and conforms to
// ToolProvider so ToolManager can route calls through it. One
// instance per registered MCP server.
type MCPProvider struct {
	spec MCPProviderSpec
	log  *slog.Logger

	mu     sync.Mutex
	client *mcpcli.Client
	closed bool

	subsMu sync.Mutex
	subs   []chan ProviderEvent
}

// NewMCPProvider spawns the MCP server, runs the protocol handshake,
// and registers tools/list_changed notifier. Caller is responsible
// for calling Close on shutdown.
func NewMCPProvider(ctx context.Context, spec MCPProviderSpec, log *slog.Logger) (*MCPProvider, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if spec.Name == "" {
		return nil, errors.New("tool: mcp spec missing name")
	}
	switch spec.Transport {
	case "", TransportStdio:
		spec.Transport = TransportStdio
		if spec.Command == "" {
			return nil, errors.New("tool: stdio mcp spec missing command")
		}
	case TransportStreamableHTTP, TransportSSE:
		if spec.Endpoint == "" {
			return nil, fmt.Errorf("tool: %s mcp spec missing endpoint", spec.Transport)
		}
	default:
		return nil, fmt.Errorf("tool: unsupported mcp transport %q (want stdio|http|sse)", spec.Transport)
	}
	p := &MCPProvider{spec: spec, log: log}
	if err := p.connect(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

// newMCPProviderWithClient is the test-only entry point that
// adopts an externally-constructed mcp-go Client (typically the
// in-process variant). It registers the tools/list_changed
// handler and runs Initialize, mirroring what NewMCPProvider does
// after spawning the stdio subprocess.
func newMCPProviderWithClient(ctx context.Context, spec MCPProviderSpec, cli *mcpcli.Client, log *slog.Logger) (*MCPProvider, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	p := &MCPProvider{spec: spec, log: log}
	cli.OnNotification(func(n mcp.JSONRPCNotification) {
		if n.Method == mcp.MethodNotificationToolsListChanged {
			p.emit(ProviderEvent{Kind: ProviderToolsChanged})
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
		return nil, fmt.Errorf("tool: initialize %s: %w", spec.Name, err)
	}
	p.client = cli
	return p, nil
}

func (p *MCPProvider) connect(ctx context.Context) error {
	cli, needsStart, err := p.newClient()
	if err != nil {
		return err
	}
	cli.OnNotification(func(n mcp.JSONRPCNotification) {
		if n.Method == mcp.MethodNotificationToolsListChanged {
			p.emit(ProviderEvent{Kind: ProviderToolsChanged})
		}
	})
	if needsStart {
		if err := cli.Start(ctx); err != nil {
			_ = cli.Close()
			return fmt.Errorf("tool: start %s: %w", p.spec.Name, err)
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
		return fmt.Errorf("tool: initialize %s: %w", p.spec.Name, err)
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
func (p *MCPProvider) newClient() (cli *mcpcli.Client, needsStart bool, err error) {
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
			return nil, false, fmt.Errorf("tool: spawn %s: %w", p.spec.Name, err)
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
			return nil, false, fmt.Errorf("tool: connect %s: %w", p.spec.Name, err)
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
			return nil, false, fmt.Errorf("tool: connect %s: %w", p.spec.Name, err)
		}
		return cli, true, nil

	default:
		return nil, false, fmt.Errorf("tool: unsupported mcp transport %q", p.spec.Transport)
	}
}

// httpClient resolves the *http.Client passed to mark3labs's HTTP
// transports. Order: explicit HTTPClient > RoundTripper-wrapped client
// > nil (mark3labs uses its default).
func (p *MCPProvider) httpClient() *http.Client {
	if p.spec.HTTPClient != nil {
		return p.spec.HTTPClient
	}
	if p.spec.RoundTripper != nil {
		return &http.Client{Transport: p.spec.RoundTripper}
	}
	return nil
}

func (p *MCPProvider) currentClient() (*mcpcli.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrProviderRemoved
	}
	if p.client == nil {
		return nil, fmt.Errorf("tool: %s not connected", p.spec.Name)
	}
	return p.client, nil
}

func (p *MCPProvider) Name() string       { return p.spec.Name }
func (p *MCPProvider) Lifetime() Lifetime { return p.spec.Lifetime }

func (p *MCPProvider) List(ctx context.Context) ([]Tool, error) {
	cli, err := p.currentClient()
	if err != nil {
		return nil, err
	}
	res, err := cli.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		if reconnErr := p.maybeReconnect(ctx, err); reconnErr == nil {
			cli, _ = p.currentClient()
			res, err = cli.ListTools(ctx, mcp.ListToolsRequest{})
		}
		if err != nil {
			return nil, fmt.Errorf("tool: list %s: %w", p.spec.Name, err)
		}
	}
	out := make([]Tool, 0, len(res.Tools))
	for _, t := range res.Tools {
		schema, _ := json.Marshal(t.InputSchema)
		out = append(out, Tool{
			Name:             p.spec.Name + ":" + t.Name,
			Description:      t.Description,
			ArgSchema:        schema,
			Provider:         p.spec.Name,
			PermissionObject: p.spec.PermObject,
		})
	}
	return out, nil
}

// Call dispatches a tool call. Args are passed verbatim; the
// caller (ToolManager) has already merged Tier-1/2 Data into
// effective args.
func (p *MCPProvider) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
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

	res, err := cli.CallTool(ctx, req)
	if err != nil {
		if reconnErr := p.maybeReconnect(ctx, err); reconnErr == nil {
			cli, _ = p.currentClient()
			res, err = cli.CallTool(ctx, req)
		}
		if err != nil {
			return nil, fmt.Errorf("tool: call %s.%s: %w", p.spec.Name, name, err)
		}
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

func (p *MCPProvider) Subscribe(ctx context.Context) (<-chan ProviderEvent, error) {
	ch := make(chan ProviderEvent, 8)
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

func (p *MCPProvider) emit(ev ProviderEvent) {
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
func (p *MCPProvider) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	cli := p.client
	p.client = nil
	p.mu.Unlock()
	if cli != nil {
		return cli.Close()
	}
	return nil
}

// maybeReconnect re-spawns the underlying stdio client if the
// returned error looks like an EOF / closed-pipe condition.
// Returns nil on a successful reconnect; the original error
// otherwise.
func (p *MCPProvider) maybeReconnect(ctx context.Context, callErr error) error {
	if callErr == nil {
		return nil
	}
	if !isEOF(callErr) {
		return callErr
	}
	p.log.Warn("mcp_provider: reconnecting after EOF", "provider", p.spec.Name, "err", callErr)
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrProviderRemoved
	}
	if p.client != nil {
		_ = p.client.Close()
		p.client = nil
	}
	p.mu.Unlock()
	if err := p.connect(ctx); err != nil {
		return err
	}
	p.emit(ProviderEvent{Kind: ProviderHealthChanged, Data: HealthHealthy})
	return nil
}

func isEOF(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "EOF") || strings.Contains(msg, "closed pipe") || strings.Contains(msg, "broken pipe")
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
