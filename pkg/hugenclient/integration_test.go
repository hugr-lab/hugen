package hugenclient

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// TestIntegration_LiveServer exercises the client against a real `hugen serve`.
// Opt-in: set HUGEN_API_URL (+ HUGEN_API_TOKEN for a keyed endpoint). The H9
// "client integration" rung of the test ladder.
//
//	HUGEN_API_URL=http://localhost:10100 go test ./pkg/hugenclient/ -run Integration -v
func TestIntegration_LiveServer(t *testing.T) {
	base := os.Getenv("HUGEN_API_URL")
	if base == "" {
		t.Skip("set HUGEN_API_URL to run against a live hugen serve")
	}
	c := New(base, WithToken(os.Getenv("HUGEN_API_TOKEN")))
	ctx := context.Background()

	u, err := c.WhoAmI(ctx)
	if err != nil {
		t.Fatalf("WhoAmI: %v", err)
	}
	t.Logf("whoami: %+v", u)

	id, err := c.CreateSession(ctx, CreateSessionOptions{Name: "integration"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	t.Logf("session: %s", id)
	defer func() { _ = c.CloseSession(ctx, id) }()

	// It must appear in the owner-scoped list.
	sessions, err := c.ListSessions(ctx, "")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	found := false
	for _, s := range sessions {
		if s.ID == id {
			found = true
		}
	}
	if !found {
		t.Errorf("created session %s not in ListSessions", id)
	}

	// Live-stream, submit a turn, and wait for at least one agent_message.
	sctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	ch, err := c.StreamLive(sctx, id)
	if err != nil {
		t.Fatalf("StreamLive: %v", err)
	}
	if err := c.SendMessage(ctx, id, "Reply with exactly the single word PONG"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	var gotAgent bool
	for ev := range ch {
		if ev.Err != nil {
			t.Fatalf("stream error: %v", ev.Err)
		}
		if ev.Frame.Kind() == protocol.KindAgentMessage {
			gotAgent = true
			break
		}
	}
	if !gotAgent {
		t.Error("no agent_message frame received from the live turn")
	}

	if evs, err := c.Events(ctx, id, EventsOptions{}); err != nil {
		t.Errorf("Events: %v", err)
	} else {
		t.Logf("events: %d", len(evs))
	}
}
