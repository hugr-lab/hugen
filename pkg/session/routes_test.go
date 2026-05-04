package session

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// TestRouteFor_DefaultsBuffered verifies kindRoutes lookups: known
// entries return their registered route, unknown kinds default to
// RouteBuffered. Pinning the table keeps phase-5 / phase-10 future
// authors honest about route choices when they add new Frame Kinds.
func TestRouteFor_DefaultsBuffered(t *testing.T) {
	cases := []struct {
		name string
		kind protocol.Kind
		want InboundRoute
	}{
		{"subagent_result is RouteToolFeed", protocol.KindSubagentResult, RouteToolFeed},
		{"subagent_started default RouteBuffered", protocol.KindSubagentStarted, RouteBuffered},
		{"whiteboard_message default RouteBuffered", protocol.KindWhiteboardMessage, RouteBuffered},
		{"agent_message default RouteBuffered", protocol.KindAgentMessage, RouteBuffered},
		{"system_message default RouteBuffered", protocol.KindSystemMessage, RouteBuffered},
		{"unknown future kind defaults RouteBuffered", protocol.Kind("future_hitl_request"), RouteBuffered},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := routeFor(tc.kind); got != tc.want {
				t.Errorf("routeFor(%q) = %v, want %v", tc.kind, got, tc.want)
			}
		})
	}
}

// TestDispatchInternal_NoHandler verifies dispatchInternal is a
// best-effort no-op for kinds without a registered handler. C6 ships
// an empty internalHandlers map, so any RouteInternal frame logged
// would hit this path; phase-4 step 10 will register handlers and
// add positive coverage at that time.
func TestDispatchInternal_NoHandler(t *testing.T) {
	s := &Session{id: "s1", logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	// A whiteboard_op frame routed internally with no handler should
	// not panic and should not mutate session state.
	frame := protocol.NewWhiteboardOp("s1", "",
		protocol.ParticipantInfo{ID: "a1", Kind: protocol.ParticipantAgent},
		protocol.WhiteboardOpPayload{Op: "init"})
	s.dispatchInternal(context.Background(), frame)
}
