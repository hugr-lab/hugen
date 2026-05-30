package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// fakeFinalizeGate is a minimal [extension.TurnFinalizeGate]: it
// vetoes a session's turn finalization the first blockTimes
// consultations (supplying continuation), then allows.
type fakeFinalizeGate struct {
	continuation string
	blockTimes   int
	consults     int
}

func (g *fakeFinalizeGate) Name() string { return "fake-gate" }

func (g *fakeFinalizeGate) GateTurnFinalize(_ context.Context, _ extension.SessionState, _ string) (string, bool) {
	g.consults++
	if g.consults <= g.blockTimes {
		return g.continuation, false
	}
	return "", true
}

func doneTurns(n int) [][]model.Chunk {
	turns := make([][]model.Chunk, n)
	for i := range turns {
		turns[i] = []model.Chunk{{Content: ptr("done"), Final: true}}
	}
	return turns
}

// TestTurnFinalizeGate_BlocksThenAllows verifies the runtime re-drives
// the SAME session when a gate vetoes finalization, injects the gate's
// continuation as a system reminder, and retires once the gate allows.
func TestTurnFinalizeGate_BlocksThenAllows(t *testing.T) {
	const cont = "submit the plan via the tool before finishing"
	mdl := &scriptedToolModel{turns: doneTurns(2)}
	gate := &fakeFinalizeGate{continuation: cont, blockTimes: 1}
	sess, cancel := newToolSessionWithExts(t, mdl, permsAllow{}, []extension.Extension{gate})
	defer cancel()

	user := protocol.ParticipantInfo{ID: "u1", Kind: protocol.ParticipantUser}
	sess.Inbox() <- protocol.NewUserMessage("s1", user, "go")

	// The turn retires only after the gate allows — wait for the
	// SECOND Final AgentMessage (the first is the gate-blocked,
	// Final=false consolidated message).
	frames := collectFrames(t, sess, func(seen []protocol.Frame) bool {
		finals := 0
		for _, f := range seen {
			if am, ok := f.(*protocol.AgentMessage); ok && am.Payload.Final && am.Payload.Consolidated {
				finals++
			}
		}
		return finals >= 1
	}, 3*time.Second)

	if gate.consults < 2 {
		t.Errorf("gate consulted %d times, want >=2 (block once + allow)", gate.consults)
	}
	if mdl.calls.Load() != 2 {
		t.Errorf("model called %d times, want 2 (re-iterate after the block)", mdl.calls.Load())
	}
	var contInjected bool
	for _, f := range frames {
		if sm, ok := f.(*protocol.SystemMessage); ok && strings.Contains(sm.Payload.Content, cont) {
			contInjected = true
		}
	}
	if !contInjected {
		t.Errorf("gate continuation not injected as a system_message: %v", kindNames(frames))
	}
}

// TestTurnFinalizeGate_RetryCapRetires verifies a gate that never
// allows can't loop forever: the runtime stops consulting past
// maxFinalizeGateRetries and retires the turn.
func TestTurnFinalizeGate_RetryCapRetires(t *testing.T) {
	mdl := &scriptedToolModel{turns: doneTurns(maxFinalizeGateRetries + 2)}
	gate := &fakeFinalizeGate{continuation: "again", blockTimes: 1 << 30} // always block
	sess, cancel := newToolSessionWithExts(t, mdl, permsAllow{}, []extension.Extension{gate})
	defer cancel()

	user := protocol.ParticipantInfo{ID: "u1", Kind: protocol.ParticipantUser}
	sess.Inbox() <- protocol.NewUserMessage("s1", user, "go")

	// Wait until the model goroutine stops being re-driven. The cap is
	// maxFinalizeGateRetries blocks → model called cap+1 times, then the
	// turn retires regardless of the gate.
	deadline := time.After(3 * time.Second)
	for {
		if mdl.calls.Load() >= int32(maxFinalizeGateRetries+1) {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("model called only %d times, want %d before retire", mdl.calls.Load(), maxFinalizeGateRetries+1)
		case <-time.After(10 * time.Millisecond):
		}
	}
	// Give the loop a beat to settle, then confirm it did NOT keep
	// going past the cap+1 call.
	time.Sleep(100 * time.Millisecond)
	if got := mdl.calls.Load(); got != int32(maxFinalizeGateRetries+1) {
		t.Errorf("model called %d times, want exactly %d (cap retires the turn)", got, maxFinalizeGateRetries+1)
	}
}
