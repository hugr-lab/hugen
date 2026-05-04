package session

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/tool"
)

// Manager satisfies tool.ToolProvider so the runtime can expose
// session-scoped tools (spawn_subagent, wait_subagents, plan_*,
// whiteboard_*, …) through the same dispatch path the LLM uses for
// every other tool. Per phase-4-spec §15 step 6 + phase-4-architecture
// §2 the provider name is "session" and the lifetime is per_agent —
// the Manager itself is per_agent (one root tree per agent) and
// per-session state lives on the *Session the dispatcher recovers
// from the context via SessionFromContext.
//
// Dispatch table sessionTools is intentionally empty in C7. Per-tool
// methods land in subsequent commits:
//
//   - C10: spawn_subagent / wait_subagents / subagent_runs /
//     subagent_cancel / parent_context.
//   - C12: plan_set / plan_comment / plan_show / plan_clear.
//   - C13: whiteboard_init / whiteboard_write / whiteboard_read /
//     whiteboard_stop.
//
// AddProvider does not require a non-empty List, so the empty
// catalogue is harmless — ToolManager.Snapshot returns the empty
// slice + the LLM never sees a "session:*" tool until C10 lands.

const sessionToolProviderName = "session"

// sessionToolHandler dispatches one Manager-provider tool call. The
// handler runs on the caller's goroutine — the *Session is recovered
// via SessionFromContext (pivot 5), and the Manager is closed over
// the registered handler via the dispatch-table closure.
type sessionToolHandler func(ctx context.Context, m *Manager, args json.RawMessage) (json.RawMessage, error)

// sessionTools is the static dispatch table. Empty in C7 — entries
// are appended as the per-tool methods land. Kept package-level +
// immutable post-init so dispatch needs no lock; new entries register
// at init() time in the file that defines them (e.g. spawn_subagent
// in pkg/session/tool_subagent.go in C10).
var sessionTools = map[string]sessionToolDescriptor{}

// sessionToolDescriptor is the runtime metadata Manager.List uses to
// project a registered tool into the tool.Tool catalogue. The schema
// is JSON-Schema-shaped (kept as raw bytes so the LLM-provider layer
// can pass it through verbatim). PermissionObject is the Tier-1 key
// the permission stack uses to gate the dispatch.
type sessionToolDescriptor struct {
	Name             string
	Description      string
	ArgSchema        json.RawMessage
	PermissionObject string
	Handler          sessionToolHandler
}

// Name implements tool.ToolProvider.
func (m *Manager) Name() string { return sessionToolProviderName }

// Lifetime implements tool.ToolProvider. session-scoped tools live
// for the duration of the agent — there's only one Manager per agent
// process, mirroring system-provider semantics.
func (m *Manager) Lifetime() tool.Lifetime { return tool.LifetimePerAgent }

// List implements tool.ToolProvider. Projects the static
// sessionTools table to []tool.Tool with the canonical
// "session:<name>" prefix the rest of ToolManager expects. Returns
// nil-slice when the table is empty (C7) — ToolManager.Snapshot
// handles this fine.
func (m *Manager) List(_ context.Context) ([]tool.Tool, error) {
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

// Call implements tool.ToolProvider. Looks up the handler in
// sessionTools by stripping the "session:" prefix and invokes it
// with the *Manager closed over. Returns ErrUnknownTool for names
// not in the dispatch table — callers see this as a tool_error
// frame upstream.
func (m *Manager) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	short := name
	if len(name) > len(sessionToolProviderName)+1 &&
		name[:len(sessionToolProviderName)+1] == sessionToolProviderName+":" {
		short = name[len(sessionToolProviderName)+1:]
	}
	d, ok := sessionTools[short]
	if !ok {
		return nil, fmt.Errorf("%w: session:%s", tool.ErrUnknownTool, short)
	}
	return d.Handler(ctx, m, args)
}

// Subscribe implements tool.ToolProvider. The session provider's
// catalogue is static (sessionTools is package-level + immutable
// after init) so there's nothing to subscribe to.
func (m *Manager) Subscribe(_ context.Context) (<-chan tool.ProviderEvent, error) {
	return nil, nil
}

// Close implements tool.ToolProvider. Manager has no provider-owned
// resources to release — its lifecycle is driven by RuntimeCore.Shutdown,
// which calls ShutdownAll separately. Returns nil so ToolManager.Close
// chains cleanly.
//
// Note: this overrides any future "tool.Closer" semantics; if Manager
// ever gains provider-private state (rare; spec doesn't call for any),
// the cleanup belongs here.
func (m *Manager) Close() error { return nil }
