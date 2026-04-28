package http

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// TestBus_SlowConsumerDrops drives the sessionBus directly to assert
// the slow-consumer drop policy fires. The HTTP-level test
// TestSSE_SlowConsumer_DropsFramesNotBlocks can't reliably trigger
// the timeout because the kernel TCP buffer absorbs many frames
// before the writeSSE goroutine's send blocks. Here we control the
// downstream consumer ourselves: subA's `out` channel is never
// drained, so after capacity (64) is exhausted the deliver timeout
// (1ms grace) fires repeatedly.
func TestBus_SlowConsumerDrops(t *testing.T) {
	upstream := make(chan protocol.Frame, 4)
	busCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// We need a parent *Adapter so bus.shutdown's deregister path
	// has somewhere to write. Buses map plus a mutex are the only
	// fields shutdown touches on the parent.
	parent := &Adapter{buses: map[string]*sessionBus{}}
	bus := &sessionBus{
		sessionID: "ses-direct",
		upstream:  upstream,
		ctx:       busCtx,
		cancel:    cancel,
		logger:    slog.Default(),
		grace:     1 * time.Millisecond,
		parent:    parent,
	}
	parent.buses[bus.sessionID] = bus
	go bus.run()
	t.Cleanup(func() { cancel() })

	// Subscriber A — never reads.
	subA := bus.addSubscriber()
	// Subscriber B — drains.
	subB := bus.addSubscriber()
	var wg sync.WaitGroup
	wg.Add(1)
	var drained atomic.Int64
	go func() {
		defer wg.Done()
		for range subB.out {
			drained.Add(1)
		}
	}()

	// Push 200 frames through. With grace=1ms and subA capacity=64,
	// the first 64 fill A's channel; the remaining 136 hit the
	// timer arm and drop.
	author := protocol.ParticipantInfo{ID: "agent-test", Kind: protocol.ParticipantAgent}
	const N = 200
	for i := 1; i <= N; i++ {
		f := protocol.NewAgentMessage("ses-direct", author, "msg", i, false)
		f.SetSeq(i)
		upstream <- f
	}

	// Wait for the bus to finish processing all frames. With 1ms
	// grace × 136 drops, the worst case is ~150ms; we give a
	// generous margin.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if drained.Load() >= int64(200-cap(subA.out)) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	drops := bus.dropCount()
	if drops <= 0 {
		t.Fatalf("expected drops > 0, got %d", drops)
	}
	if int(drops) > N {
		t.Errorf("drop count %d exceeds frame count %d", drops, N)
	}
	t.Logf("drops=%d, B drained=%d", drops, drained.Load())

	// Tear down — signal bus.run to exit so the test cleans up.
	cancel()
	wg.Wait()
}
