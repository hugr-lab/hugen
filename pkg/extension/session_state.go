package extension

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/prompts"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// SessionState is the typed-handle bag every per-session
// extension and tool provider reaches into. Implementations
// (today: *session.Session, test fakes) own the underlying
// concurrency; callers just SetValue / Value through the
// interface.
//
// Extensions store per-session projections under a stable name
// (typically the extension's own [Extension.Name]). Sub-agents
// reach the host's state through [SessionState.Parent] — call
// SessionID / Value on the returned parent directly.
//
// Tools returns the per-session child [*tool.ToolManager] —
// extensions that need to mount providers dynamically at runtime
// (skill_load reading a manifest's allowed-tools and registering
// the matching MCP) call AddProvider on the result. The returned
// manager's lifetime is the session's; closing it is the
// session's job, not the caller's.
type SessionState interface {
	SessionID() string
	Value(name string) (any, bool)
	SetValue(name string, value any)

	// Depth returns this session's depth in the spawn tree: 0 for
	// the user-facing root, 1 for the mission root spawned, ≥2 for
	// workers spawned by missions (or by workers via opt-in
	// can_spawn). The tier vocabulary (root/mission/worker) is
	// derived from depth via skill.TierFromDepth. Phase 4.2.2 §2.
	Depth() int

	// Parent returns the parent session's state for spawned
	// sessions and (nil, false) for root sessions. Callers read
	// parent's SessionID / Value directly off the returned handle
	// — transparently traverses any depth.
	Parent() (SessionState, bool)

	// Children returns a snapshot of the direct child sessions'
	// states, or nil for sessions with no children. The returned
	// slice is safe for the caller to iterate; new spawns after the
	// call are not reflected. Used by extensions that fan a frame
	// out to every member of a group (whiteboard host broadcast).
	Children() []SessionState

	Tools() *tool.ToolManager

	// Prompts returns the agent-level template renderer shared
	// by every session in the tree. Used by extensions whose
	// AdvertiseSystemPrompt / PerTurnPrompt surfaces produce
	// model-visible prose from bundled templates under
	// assets/prompts/. May be nil only in test fixtures that
	// skip the renderer — production paths assume non-nil.
	Prompts() *prompts.Renderer

	// Emit persists frame on the calling session's event log and
	// pushes it through the session's outbox for adapters.
	// Extensions emitting state-change events ([protocol.ExtensionFrame]
	// with Category=Op) call this so Recovery can replay them on
	// restart.
	Emit(ctx context.Context, frame protocol.Frame) error

	// IsClosed reports whether the session has begun teardown
	// (closed flag set, Run goroutine exiting / exited). Useful
	// for callers that want to distinguish "delivered" from
	// "session gone" after awaiting a [SessionState.Submit]
	// channel — the channel itself is close-only and doesn't
	// carry that signal.
	IsClosed() bool

	// Submit delivers frame to this session's inbox
	// asynchronously and returns a "settled" channel that closes
	// when the send has either landed in the inbox, the session
	// terminated, or ctx fired. Callers that want
	// fire-and-forget ignore the returned channel; callers that
	// need delivery before proceeding wait on it. The channel
	// does not distinguish delivered from cancelled — use
	// post-checks (IsClosed / ctx.Err()) when that distinction
	// matters. Used by extensions that route frames across
	// sessions in the tree (member→host whiteboard write,
	// host→member broadcast) where blocking on a slow consumer
	// would stall the producer's Run goroutine.
	Submit(ctx context.Context, frame protocol.Frame) <-chan struct{}

	// OutboxOnly publishes frame on the session's outbox without
	// persisting it to session_events (no AppendEvent, no seq
	// alloc). Used by extensions that produce transient
	// observability frames (e.g. liveview status updates) — they
	// flow to live adapter subscribers but don't pollute the
	// event log. Mirrors the internal Session.outboxOnly path
	// already used by phase-5.1 InquiryRequest bubble. Phase 5.1b.
	OutboxOnly(ctx context.Context, frame protocol.Frame) error

	// Extensions returns the agent-level slice of registered
	// extensions in deterministic order. Used by aggregators
	// (notably liveview) that need to iterate every sibling
	// extension on the session and call a capability (e.g.
	// [StatusReporter]) without hardcoding extension names.
	// Phase 5.1b.
	Extensions() []Extension
}

type sessionStateKey struct{}

// WithSessionState attaches state to ctx. session.Session calls
// this before tool dispatch so handlers downstream can recover
// the calling session's typed state.
func WithSessionState(ctx context.Context, state SessionState) context.Context {
	return context.WithValue(ctx, sessionStateKey{}, state)
}

// SessionStateFromContext returns the state attached via
// [WithSessionState], or (nil, false) if no state is present.
// Tool providers and extension handlers use this to recover the
// calling session's typed state from the dispatch ctx.
func SessionStateFromContext(ctx context.Context) (SessionState, bool) {
	s, ok := ctx.Value(sessionStateKey{}).(SessionState)
	return s, ok
}
