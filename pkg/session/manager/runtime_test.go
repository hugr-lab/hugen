package manager

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
)

func liveStatusFrame(sid string) *protocol.ExtensionFrame {
	return protocol.NewExtensionFrame(
		sid,
		protocol.ParticipantInfo{ID: "liveview", Kind: protocol.ParticipantSystem, Name: "liveview"},
		"liveview", protocol.CategoryMarker, "status", json.RawMessage(`{"lifecycle_state":"active"}`),
	)
}

func TestIsLiveviewStatus(t *testing.T) {
	if !isLiveviewStatus(liveStatusFrame("ses-1")) {
		t.Error("liveview status frame not detected")
	}
	other := protocol.NewExtensionFrame("ses-1",
		protocol.ParticipantInfo{ID: "plan", Kind: protocol.ParticipantSystem, Name: "plan"},
		"plan", protocol.CategoryMarker, "set", json.RawMessage(`{}`))
	if isLiveviewStatus(other) {
		t.Error("non-liveview extension frame mis-detected as status")
	}
}

func newTestRuntime() *Runtime {
	return &Runtime{
		logger:      slog.Default(),
		subscribers: make(map[string][]chan protocol.Frame),
		lastStatus:  make(map[string]protocol.Frame),
		pumping:     make(map[*session.Session]bool),
		ctx:         context.Background(),
	}
}

// TestSubscribePrimesCachedStatus: a session that has emitted a liveview status
// primes a fresh subscriber with it immediately (the status is outbox-only and
// otherwise wouldn't reach a late-attaching client).
func TestSubscribePrimesCachedStatus(t *testing.T) {
	r := newTestRuntime()
	host := &adapterHost{rt: r, ctx: context.Background()}
	const sid = "ses-1"

	r.fanout(liveStatusFrame(sid)) // caches lastStatus[sid]

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := host.Subscribe(ctx, sid)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	select {
	case f := <-ch:
		if !isLiveviewStatus(f) || f.SessionID() != sid {
			t.Errorf("primed frame = %+v, want liveview status for %s", f, sid)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber was not primed with the cached status")
	}
}

// TestSubscribeNoStatusNotPrimed: a session with no prior status leaves a fresh
// subscriber empty (nothing to prime).
func TestSubscribeNoStatusNotPrimed(t *testing.T) {
	r := newTestRuntime()
	host := &adapterHost{rt: r, ctx: context.Background()}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := host.Subscribe(ctx, "ses-none")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	select {
	case f := <-ch:
		t.Errorf("unexpected primed frame on an un-cached session: %+v", f)
	case <-time.After(50 * time.Millisecond):
		// expected: nothing primed
	}
}
