package session

import "github.com/hugr-lab/hugen/pkg/protocol"

// InboundRoute classifies how the Run loop handles a Frame arriving on
// s.in. Per phase-4-spec §10.2, every Frame falls into exactly one
// route, table-driven via kindRoutes.
//
// Control frames (Cancel, SlashCommand, UserMessage) bypass the table
// and stay inline in routeInbound: they're the session-lifecycle
// triggers, not session-to-session data, and the spec's three-route
// model targets the latter (subagent_*, whiteboard_*, future hitl_*).
// New Frame kinds carrying multi-session data MUST register here or
// fall through to RouteBuffered.
type InboundRoute int

const (
	// RouteBuffered is the default for every unregistered Frame Kind.
	// Buffered frames append to s.pendingInbound and drain into
	// s.history at the next turn boundary (per §10.3 + §11 visibility
	// filter).
	RouteBuffered InboundRoute = iota

	// RouteInternal triggers a synchronous side-effect handler from
	// internalHandlers — runs immediately (even mid-turn), persists
	// any events the handler chooses, and discards the Frame: it
	// never reaches s.history. Phase 4 reserves this for whiteboard
	// host-side ops (kinds added in step 10); the table stays empty
	// until then.
	RouteInternal

	// RouteToolFeed forwards the Frame to s.activeToolFeed when one
	// is registered AND its Consumes predicate matches. Otherwise
	// falls back to RouteBuffered. The canonical phase-4 user is
	// wait_subagents (step 7), which registers a feed for
	// subagent_result while the tool blocks.
	RouteToolFeed
)

// kindRoutes maps Frame Kind to its routing category. Entries omitted
// from this map default to RouteBuffered.
//
// Phase 4 only seeds RouteToolFeed for subagent_result (consumed by
// wait_subagents in C7). Whiteboard host-side kinds register here in
// step 10 with RouteInternal once protocol gains the matching Kind
// constants. Phase 5 HITL kinds register on landing.
var kindRoutes = map[protocol.Kind]InboundRoute{
	protocol.KindSubagentResult: RouteToolFeed,
}

// routeFor looks up the InboundRoute for a Frame Kind. Default is
// RouteBuffered, so unknown / future kinds get a sane treatment
// without a panic.
func routeFor(k protocol.Kind) InboundRoute {
	if r, ok := kindRoutes[k]; ok {
		return r
	}
	return RouteBuffered
}
