// Command hugr-query is the in-tree Hugr GraphQL MCP server,
// spawned by the runtime over stdio (per-agent lifetime). It
// exposes hugr.Query (writes Parquet for tabular results, JSON
// for objects) and hugr.QueryJQ (post-processes via JQ before
// writing one JSON value).
//
// Auth uses the unmodified pkg/auth/sources/hugr.Source
// configured with TokenURL pointing at the agent's
// /api/auth/agent-token endpoint and a per-spawn bootstrap token
// passed via HUGR_ACCESS_TOKEN env. The agent enforces a 30 s
// bootstrap window plus a per-spawn IssuedHistory LRU so the MCP
// can rotate transparently when the agent's underlying Hugr token
// rotates.
//
// Per-call timeout is read from args.timeout_ms, clamped to
// HUGR_QUERY_MAX_TIMEOUT_MS, with HUGR_QUERY_TIMEOUT_MS as the
// default. Actual elapsed_ms is reported in every success
// envelope.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"

	"github.com/hugr-lab/query-engine/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log); err != nil {
		log.Error("hugr-query: bootstrap failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	authCfg, err := loadAuthConfig()
	if err != nil {
		return err
	}
	timeouts := loadTimeoutConfig()
	tokenSrc := buildTokenSource(authCfg)

	// HUGR_URL is the literal IPC endpoint — query-engine/client
	// POSTs to this URL verbatim for c.Query (multipart wire), and
	// derives c.QueryJSON's URL from it by trimming `/ipc` and
	// appending `/query` / `/jq-query`. The operator writes the
	// full path in config; we don't second-guess it (some deploys
	// front Hugr with a proxy on a non-standard prefix).
	cli := client.NewClient(
		authCfg.HugrURL,
		client.WithTransport(buildTransport(tokenSrc)),
	)

	deps := &queryDeps{
		client:    cli,
		timeouts:  timeouts,
		workspace: os.Getenv("WORKSPACES_ROOT"),
	}

	srv := server.NewMCPServer(
		"hugr-query",
		"phase-3",
		server.WithToolCapabilities(true),
	)
	registerTools(srv, deps)

	log.Info("hugr-query: starting stdio server",
		"hugr_url", authCfg.HugrURL,
		"token_url", authCfg.TokenURL,
		"workspaces_root", deps.workspace,
		"timeout_default_ms", timeouts.DefaultMS,
		"timeout_max_ms", timeouts.MaxMS,
	)
	_ = http.DefaultClient // keep import lively for future header injection
	return server.ServeStdio(srv)
}

// registerTools wires hugr.Query and hugr.QueryJQ onto the MCP
// server. Each handler reads session_id from the per-call MCP
// metadata (`_meta.session_id`); the runtime injects it before
// dispatch. Direct callers (tests) populate the same key.
func registerTools(srv *server.MCPServer, deps *queryDeps) {
	srv.AddTool(mcp.NewTool("query",
		mcp.WithDescription(`Run a GraphQL query against Hugr and persist every result part to disk under the session workspace. Use this for big result sets and anything you intend to read back later via bash tools — for small inline-friendly results prefer hugr-main:data-* tools.

Per-part format is decided by the response shape, not by the caller:
  - tabular parts (ArrowTable) → Parquet
  - object / scalar parts      → JSON
  - GraphQL "extensions"       → JSON (when present)

Each part is named after its GraphQL field path (dots preserved as ` + "`_`" + `), e.g. ` + "`function_core_payments_aggregation.parquet`" + `. The ` + "`path`" + ` argument names the *directory* the parts land in.

Result envelope:
  { "query_id":   string,           // opaque id of this call
    "elapsed_ms": int,               // actual wall time
    "part":       partEntry,         // present when the response has exactly one part
    "parts":      [partEntry, ...]   // present when the response has > 1 part
  }

partEntry:
  { "path":      string,             // absolute on-disk path (omitted when "null": true)
    "format":    "parquet"|"json",
    "field":     string,             // dotted GraphQL path the part came from
                                     //   e.g. "function.core.payments.aggregation"
    "size":      int,                // bytes written
    "row_count": int,                // Parquet only
    "schema":    [{name,type,metadata?}, ...], // Parquet only — Arrow schema
    "preview":   string,             // JSON only — first ≤ 1 KB of the file body
    "truncated": bool,               // JSON only — true when preview was capped
    "null":      bool                // part was null/empty; no file written
  }

Empty / null parts are NOT written to disk — the entry just carries field+null:true so you still see the shape of the response. Read full file contents back via bash-mcp when needed.`),
		mcp.WithString("graphql", mcp.Required(), mcp.Description("Full GraphQL query text.")),
		mcp.WithObject("variables", mcp.Description("GraphQL variables.")),
		mcp.WithString("path", mcp.Description(
			`Optional relative directory (inside your session workspace) where parts will be written. `+
				`Each part lands in this directory under a filename derived from its GraphQL path `+
				`(e.g. "function_core_payments_aggregation.parquet"). `+
				`Output never leaves the session workspace — if you need the files in the agent's shared `+
				`folder, copy them there afterwards via bash-mcp. `+
				`Default (omitted): "data/<query_id>/".`)),
		mcp.WithNumber("timeout_ms", mcp.Description("Per-call deadline in ms. Silently clamped to the operator ceiling (HUGR_QUERY_MAX_TIMEOUT_MS, default 24 h). Defaults to HUGR_QUERY_TIMEOUT_MS (default 1 h).")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args queryArgs
		if err := req.BindArguments(&args); err != nil {
			return errResult(&toolError{Code: "arg_validation", Msg: err.Error()}), nil
		}
		sid := sessionIDFromRequest(req)
		out, err := deps.runQuery(ctx, sid, args)
		if err != nil {
			return errResult(err), nil
		}
		return okResult(out)
	})

	srv.AddTool(mcp.NewTool("query_jq",
		mcp.WithDescription("Run a GraphQL query, transform the result with JQ server-side, persist a single JSON value under the session workspace. The JQ input is the full {data, errors} response envelope — typically you start with .data."),
		mcp.WithString("graphql", mcp.Required(), mcp.Description("Full GraphQL query text.")),
		mcp.WithString("jq", mcp.Required(), mcp.Description("JQ expression. Input has the GraphQL response shape: {\"data\": {...}, \"errors\": [...]}. Reach query results via .data.<field>.")),
		mcp.WithObject("variables"),
		mcp.WithString("path", mcp.Description("Output path. Relative paths anchor at the session workspace root. Default: data/<short_id>.json.")),
		mcp.WithNumber("timeout_ms"),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args queryJQArgs
		if err := req.BindArguments(&args); err != nil {
			return errResult(&toolError{Code: "arg_validation", Msg: err.Error()}), nil
		}
		sid := sessionIDFromRequest(req)
		out, err := deps.runQueryJQ(ctx, sid, args)
		if err != nil {
			return errResult(err), nil
		}
		return okResult(out)
	})
}

// sessionIDFromRequest extracts the runtime-supplied session id
// from MCP per-call metadata. The runtime puts it under
// `_meta.session_id`. Tests can put it directly into AdditionalFields.
func sessionIDFromRequest(req mcp.CallToolRequest) string {
	if req.Params.Meta == nil {
		return ""
	}
	if v, ok := req.Params.Meta.AdditionalFields["session_id"]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// okResult marshals a queryResult into a CallToolResult. The MCP
// client deserialises StructuredContent automatically.
func okResult(out queryResult) (*mcp.CallToolResult, error) {
	body, err := json.Marshal(out)
	if err != nil {
		return errResult(&toolError{Code: "io", Msg: err.Error()}), nil
	}
	res := mcp.NewToolResultText(string(body))
	res.StructuredContent = out
	return res, nil
}

// errResult wraps a tool error in the MCP isError shape and
// embeds the structured envelope in the text content so callers
// that don't read structured fields still see the code.
func errResult(err error) *mcp.CallToolResult {
	var te *toolError
	if !errors.As(err, &te) {
		te = &toolError{Code: "io", Msg: err.Error()}
	}
	body, _ := json.Marshal(te)
	res := mcp.NewToolResultErrorf("%s", string(body))
	res.StructuredContent = te
	return res
}
