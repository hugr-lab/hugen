// Package extension defines the contract for pluggable session
// extensions: discrete components (plan, whiteboard, skills,
// notepad, future plugins) the runtime composes onto every session
// at boot. An extension is a normal Go value; the capability
// interfaces below describe the pipelines the runtime hooks it
// into. An extension does not need to implement all of them — the
// runtime asserts each capability and wires it where it applies.
//
// Extensions are agent-level singletons constructed once during
// runtime boot (pkg/runtime). Per-session state lives in a
// [SessionState] handle the extension stores via SetValue at
// session creation; subsequent calls (tool dispatch, prompt
// rendering, frame routing) read it via Value.
//
// See pkg/protocol.ExtensionFrame for the envelope through which
// extension-defined events flow on the persistence layer and the
// transport.
package extension

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// Extension is the marker interface every plug-in implements.
// Name returns a stable identifier (e.g. "plan", "whiteboard",
// "skill") used as the namespace key in [SessionState] and as
// the routing discriminator on [protocol.ExtensionFrame]. Names are
// case-sensitive and must be unique across the registered set; the
// runtime panics on a duplicate registration.
type Extension interface {
	Name() string
}

// StateInitializer extensions create their per-session state at
// session construction. Called once from session.NewSession after
// the shell is built and before the goroutine starts (so the
// handle is observable to the very first inbound frame). Idempotent
// is not required — InitState runs exactly once per session.
//
// Implementations typically allocate their projection struct and
// call state.SetValue(ext.Name(), handle); subsequent capability
// calls retrieve it via state.Value(ext.Name()).
type StateInitializer interface {
	InitState(ctx context.Context, state SessionState) error
}

// Recovery extensions rebuild their per-session state from the
// session's event log. Called lazily on the first inbound frame
// (materialise) — once per session lifetime. The state handle is
// already populated by InitState; Recovery mutates it by replaying
// matching events. Implementations filter the rows by
// EventType/Extension/Op as appropriate; the runtime hands over
// the full event slice without pre-filtering so a single pass over
// the log suffices for every Recovery extension.
//
// Returning an error logs a warning; recovery is best-effort and
// must not block session start.
type Recovery interface {
	Recover(ctx context.Context, state SessionState, events []store.EventRow) error
}

// Closer extensions release per-session resources at session
// teardown. Called from session.teardown after lifecycle.Release
// and before the per-session ToolManager closes. Errors are logged
// but do not abort teardown — close paths must drain regardless.
//
// Optional: extensions whose state is plain memory (plan,
// whiteboard projections) skip this; only extensions that hold
// goroutines, file handles, or external bindings implement it.
type Closer interface {
	Close(ctx context.Context, state SessionState) error
}

// Advertiser extensions contribute a section to the system prompt
// the runtime feeds into the model on each turn. The returned
// string is concatenated into the prompt verbatim (with newline
// separation). An empty string skips the section. Sections are
// emitted in extension-registration order; v1 does not provide
// ordering primitives — order this matters to is documented at the
// registration site.
type Advertiser interface {
	AdvertiseSystemPrompt(ctx context.Context, state SessionState) string
}

// ToolFilter extensions narrow the per-session tool catalogue.
// Called by Session.Snapshot when the cached snapshot is rebuilt
// (cache invalidates on toolGen / policyGen / extension generation
// bumps). Multiple filters compose by intersection — the most
// restrictive wins. Implementations must be deterministic for a
// given (state, all) pair so the snapshot cache stays correct.
type ToolFilter interface {
	FilterTools(ctx context.Context, state SessionState, all []tool.Tool) []tool.Tool
}

// FrameRouter extensions handle inbound [protocol.ExtensionFrame]
// addressed to them (Frame.Extension == ext.Name()). The session's
// route loop dispatches by Extension name; each name maps to at
// most one router. Returning an error surfaces as a warning in the
// session log; the frame is considered consumed regardless.
//
// Extensions that emit ExtensionFrames but never consume them
// (plan, skill) skip this interface — only consumers implement it
// (today: whiteboard's broadcast member-side handler).
type FrameRouter interface {
	HandleFrame(ctx context.Context, state SessionState, f *protocol.ExtensionFrame) error
}
