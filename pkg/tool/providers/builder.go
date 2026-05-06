package providers

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/tool"
	"github.com/hugr-lab/hugen/pkg/tool/providers/mcp"
	"github.com/hugr-lab/hugen/pkg/tool/providers/recovery"
)

// Builder is the concrete tool.ProviderBuilder implementation.
// It dispatches Spec values to type-specific subpackages by
// Spec.Type — empty / "mcp" routes to pkg/tool/providers/mcp.
//
// Builder is constructed once at boot (pkg/runtime in stage B)
// with explicit dependencies; ToolManager calls Build per
// registered Spec and registers the returned provider.
type Builder struct {
	auth          *auth.Service
	perms         perm.Service
	workspaceRoot string
	logger        *slog.Logger
}

// NewBuilder constructs a Builder. Required dependencies are
// passed explicitly — there is no Deps wrapper struct, and adding
// a new dependency is a deliberate signature change.
//
// Args:
//   - authSvc: handles spec.Auth (HTTP RoundTripper for HTTP/SSE,
//     stdio bootstrap mint for stdio). nil disables auth-bound
//     specs (they fail at Build time).
//   - perms: phase-4.1a does not consume perms inside the Builder
//     itself; the parameter is reserved for the future webhook /
//     kubernetes provider types that need permission gating at
//     spawn time. Pass perm.Service from the Manager's surface.
//   - workspaceRoot: absolute path injected into stdio children
//     as WORKSPACES_ROOT so per_agent and per_session subprocesses
//     share the same on-disk tree. Empty disables injection.
//   - logger: forwarded into each provider's wire-level logger.
//     nil falls back to slog.New(slog.DiscardHandler) inside the
//     subpackage.
func NewBuilder(authSvc *auth.Service, perms perm.Service, workspaceRoot string, logger *slog.Logger) *Builder {
	return &Builder{
		auth:          authSvc,
		perms:         perms,
		workspaceRoot: workspaceRoot,
		logger:        logger,
	}
}

// Build dispatches by Spec.Type. Empty type defaults to "mcp"
// (back-compat with the original tool_providers schema). Adding
// a new provider type is two lines: import the subpackage at
// the top of this file and add a case below.
//
// MCP providers are wrapped with recovery.Wrap (design-001
// §6.7b): the inner *mcp.Provider implements tool.Recoverable
// via TryReconnect, and the recovery decorator drives the retry
// loop on failed Call/List. Other provider types choose to wrap
// or not at the case level — system providers (admin, policies,
// runtime:reload, session:*) skip the wrap because they have
// nothing to reconnect.
func (b *Builder) Build(ctx context.Context, spec tool.Spec) (tool.ToolProvider, error) {
	t := strings.ToLower(spec.Type)
	if t == "" {
		t = "mcp"
	}
	switch t {
	case "mcp":
		prov, err := mcp.New(ctx, spec, b.auth, b.workspaceRoot, b.logger)
		if err != nil {
			return nil, err
		}
		return recovery.Wrap(prov, recovery.WithLogger(b.logger)), nil
	default:
		return nil, fmt.Errorf("providers: unknown type %q", spec.Type)
	}
}

// ensure Builder implements the new tool.ProviderBuilder contract.
var _ tool.ProviderBuilder = (*Builder)(nil)
