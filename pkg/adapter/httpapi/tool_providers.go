package httpapi

// The tool-provider panel surface (backs the hub console's per-agent MCP view):
//
//	GET  /v1/tool-providers         → the agent's managed (per_agent HTTP/SSE
//	                                  MCP) providers, each flagged live
//	POST /v1/tool-providers/reload  → reconcile the root ToolManager from the
//	                                  freshly re-fetched agent config (add /
//	                                  remove / replace changed) — the hub POSTs
//	                                  this after persisting a config_override edit
//
// Providers are always per_agent (on the root ToolManager), so a reload becomes
// visible to every live session on its next turn (the root generation bump folds
// into each session's child manager). The owner/admin gate on mutations is
// enforced upstream at the hub; these handlers authenticate the forwarded user.

import (
	"context"
	"net/http"
	"time"
)

// toolProviderReloadTimeout bounds a reconcile (each new provider connects +
// runs the MCP Initialize handshake) so an unreachable server can't hang it.
const toolProviderReloadTimeout = 90 * time.Second

// ToolProviderLister returns the agent's managed tool providers as a
// JSON-marshalable value. nil ⇒ GET /v1/tool-providers returns 501.
type ToolProviderLister func(ctx context.Context) (any, error)

// ToolProviderReloader reconciles the root ToolManager's managed providers from
// the re-fetched agent config and returns a compact outcome. nil ⇒ 501.
type ToolProviderReloader func(ctx context.Context) (any, error)

// WithToolProviderLister enables GET /v1/tool-providers.
func WithToolProviderLister(fn ToolProviderLister) Option {
	return func(a *Adapter) { a.listToolProviders = fn }
}

// WithToolProviderReloader enables POST /v1/tool-providers/reload.
func WithToolProviderReloader(fn ToolProviderReloader) Option {
	return func(a *Adapter) { a.reloadToolProviders = fn }
}

func (a *Adapter) handleListToolProviders(w http.ResponseWriter, r *http.Request) {
	if a.listToolProviders == nil {
		httpError(w, http.StatusNotImplemented, "tool providers not available")
		return
	}
	out, err := a.listToolProviders(r.Context())
	if err != nil {
		a.logger.Warn("httpapi: list tool providers", "err", err)
		httpError(w, http.StatusBadGateway, "list tool providers failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *Adapter) handleReloadToolProviders(w http.ResponseWriter, r *http.Request) {
	if a.reloadToolProviders == nil {
		httpError(w, http.StatusNotImplemented, "tool providers not available")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), toolProviderReloadTimeout)
	defer cancel()
	out, err := a.reloadToolProviders(ctx)
	if err != nil {
		a.logger.Warn("httpapi: reload tool providers", "err", err)
		httpError(w, http.StatusBadGateway, "reload tool providers failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}
