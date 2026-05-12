package session

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// newStuckTestSession builds the bare minimum *Session a stuck-edge
// unit test needs: a closed session (so emit short-circuits without
// touching agent/store) and a discard logger (so the Warn after the
// short-circuit doesn't panic). Deps carries only the Prompts
// renderer — the detector calls MustRender to build nudge text. The
// detector code under test mutates stuck.* fields BEFORE calling
// emit, so the flag transitions still surface correctly even though
// no Frame ever lands in the store.
func newStuckTestSession(t *testing.T) *Session {
	agent, _ := NewAgent("a1", "hugen", &fakeIdentity{id: "a1"}, "", nil)
	s := &Session{
		id:     "s1",
		agent:  agent,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		deps:   &Deps{Prompts: testPrompts(t)},
	}
	s.closed.Store(true)
	return s
}

// TestStuckDetector_RepeatedHashRisingEdge: three identical hashes in
// a row flip the detector to active and emit one nudge; a different
// hash clears the flag; three more identical calls fire it again
// (phase-4-spec §13.2 #6).
func TestStuckDetector_RepeatedHashRisingEdge(t *testing.T) {
	s := newStuckTestSession(t)
	now := time.Now()

	// Two identical calls — not enough for a rising edge yet.
	s.stuckObserveCall("fake:do", map[string]any{"x": 1}, "", now)
	s.stuckObserveCall("fake:do", map[string]any{"x": 1}, "", now.Add(50*time.Millisecond))
	s.evaluateRepeatedHash(nilCtx())
	if s.stuck.repeatedHashActive {
		t.Fatalf("active after 2 identical calls; need 3")
	}

	// Third call → rising edge.
	s.stuckObserveCall("fake:do", map[string]any{"x": 1}, "", now.Add(100*time.Millisecond))
	s.evaluateRepeatedHash(nilCtx())
	if !s.stuck.repeatedHashActive {
		t.Fatalf("rising edge missed on third identical call")
	}

	// Different hash breaks the run.
	s.stuckObserveCall("fake:do", map[string]any{"x": 2}, "", now.Add(150*time.Millisecond))
	s.evaluateRepeatedHash(nilCtx())
	if s.stuck.repeatedHashActive {
		t.Fatalf("flag should clear when pattern breaks (different hash in tail)")
	}

	// Three new identical calls re-fire.
	s.stuckObserveCall("fake:do", map[string]any{"x": 2}, "", now.Add(200*time.Millisecond))
	s.stuckObserveCall("fake:do", map[string]any{"x": 2}, "", now.Add(250*time.Millisecond))
	// note: the trailing window now holds [x=2, x=2, x=2] after evaluation
	s.evaluateRepeatedHash(nilCtx())
	if !s.stuck.repeatedHashActive {
		t.Fatalf("recurrence should fire the rising edge again after pattern break")
	}
}

// TestSessionToolHash_StableAcrossCalls is a sanity check on the
// fallback hash used for providers that don't compute Hash inline:
// equal (name, args) ⇒ equal hash; different ⇒ different hash. Without
// this property the rising-edge detectors are toothless when the
// scripted test models don't fill in ChunkToolCall.Hash.
func TestSessionToolHash_StableAcrossCalls(t *testing.T) {
	a := sessionToolHash("fake:do", map[string]any{"x": 1, "y": "z"})
	b := sessionToolHash("fake:do", map[string]any{"x": 1, "y": "z"})
	if a == "" || a != b {
		t.Errorf("hash unstable: a=%q b=%q", a, b)
	}
	c := sessionToolHash("fake:do", map[string]any{"x": 2})
	if c == a {
		t.Errorf("hash collision on different args: %q", c)
	}
	d := sessionToolHash("other:do", map[string]any{"x": 1, "y": "z"})
	if d == a {
		t.Errorf("hash collision on different tool name: %q", d)
	}
}

// TestStuckBuffer_FIFOTrim asserts the trailing window stays bounded
// at max(stuckRepeatedHashWindow, stuckTightDensityCount). Without the
// trim the window would grow unbounded across a long session.
func TestStuckBuffer_FIFOTrim(t *testing.T) {
	s := newStuckTestSession(t)
	now := time.Now()
	for i := 0; i < 50; i++ {
		s.stuckObserveCall("fake:do", map[string]any{"i": i}, "", now)
	}
	want := stuckRepeatedHashWindow
	if stuckTightDensityCount > want {
		want = stuckTightDensityCount
	}
	if got := len(s.stuck.recentHashes); got != want {
		t.Errorf("recentHashes len = %d, want trim to %d", got, want)
	}
	if got := len(s.stuck.recentErrored); got != want {
		t.Errorf("recentErrored len = %d, want trim to %d", got, want)
	}
}

// nilCtx is a tiny convenience for the rising-edge tests above; the
// detectors only call s.emit on the rising edge, and emit short-
// circuits on s.closed=true (set by newStuckTestSession), so the ctx
// is never actually consulted in this code path.
func nilCtx() context.Context { return context.Background() }
