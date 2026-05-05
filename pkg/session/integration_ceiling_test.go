package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// integration_ceiling_test.go covers phase-4-spec §13.1 + §13.2 #7
// (soft-warning happy path) and §13.2 #8 (hard-ceiling termination)
// for a root session driven by a scriptedToolModel that legitimately
// wants more turns than max_turns. Sub-agent surfacing is exercised
// through the parent-receives-subagent_result path of the existing
// US1 cancel suite; this file focuses on the local ceiling decision.

// fanOutToolModel emits one tool_call per turn (with a per-turn distinct
// arg so the stuck-detection rising edge does NOT fire) until it has
// burned `turns` turns; then it returns final content. Used by both
// soft-warning and hard-ceiling tests below — the only difference is
// the configured caps.
type fanOutToolModel struct {
	turns int
	calls int
	mu    chan struct{} // serialises the calls counter without sync.Mutex import noise.
}

func newFanOutToolModel(turns int) *fanOutToolModel {
	m := &fanOutToolModel{turns: turns, mu: make(chan struct{}, 1)}
	m.mu <- struct{}{}
	return m
}

func (m *fanOutToolModel) Spec() model.ModelSpec {
	return model.ModelSpec{Provider: "fake", Name: "fanout"}
}

func (m *fanOutToolModel) Generate(_ context.Context, _ model.Request) (model.Stream, error) {
	<-m.mu
	idx := m.calls
	m.calls++
	m.mu <- struct{}{}

	if idx >= m.turns {
		ch := make(chan model.Chunk, 1)
		final := "done"
		ch <- model.Chunk{Content: &final, Final: true}
		close(ch)
		return &scriptedStream{ch: ch}, nil
	}
	ch := make(chan model.Chunk, 1)
	ch <- model.Chunk{ToolCall: &model.ChunkToolCall{
		ID:   idxID(idx),
		Name: "fake:do",
		// distinct args per turn so repeated_hash never trips and the
		// nudge tests stay focused on the soft/hard ceilings.
		Args: map[string]any{"step": idx},
	}}
	close(ch)
	return &scriptedStream{ch: ch}, nil
}

func idxID(i int) string { return "tc-" + string(rune('A'+i%26)) }

// newCeilingTestManager builds a Manager that routes every session
// through (mdl, provider) with the requested soft/hard caps wired
// onto every spawned Session via WithSessionOptions. Mirrors
// newTestManager but with the explicit dep overrides the §8 tests
// need.
func newCeilingTestManager(t *testing.T, store RuntimeStore, mdl model.Model, provider tool.ToolProvider, softCap, hardCap int) *Manager {
	t.Helper()
	tm := tool.NewToolManager(permsAllow{}, nil, nil, nil)
	if provider != nil {
		if err := tm.AddProvider(provider); err != nil {
			t.Fatalf("AddProvider: %v", err)
		}
	}
	router := newRouterWithModel(t, mdl)
	agent, err := NewAgent("a1", "hugen", &fakeIdentity{id: "a1"}, "")
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	return NewManager(store, agent, router, NewCommandRegistry(), protocol.NewCodec(), nil,
		WithSessionOptions(
			WithTools(tm),
			WithMaxToolIterations(softCap),
			WithMaxToolIterationsHard(hardCap),
		))
}

// TestRoot_SoftWarning_FiresOncePastCap (§13.1 + §13.2 #7): cap_soft=3,
// hard ceiling far above the model's planned turn count. On the 4th
// iteration the runtime injects exactly one
// system_message{kind:"soft_warning"} into the session's events;
// subsequent iterations do not re-inject.
func TestRoot_SoftWarning_FiresOncePastCap(t *testing.T) {
	const softCap = 3
	store := newFakeStore()
	mdl := newFanOutToolModel(5) // 5 tool turns + final
	provider := &stubProvider{
		tools:  []tool.Tool{{Name: "fake:do", Provider: "fake", PermissionObject: "hugen:tool:fake"}},
		result: `{"ok":true}`,
	}
	mgr := newCeilingTestManager(t, store, mdl, provider, softCap, 64)
	defer mgr.ShutdownAll(context.Background())

	root, _, err := mgr.Open(context.Background(), OpenRequest{OwnerID: "alice"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	drainOutboxOnce(root.Outbox())

	user := protocol.ParticipantInfo{ID: "u1", Kind: protocol.ParticipantUser}
	root.Inbox() <- protocol.NewUserMessage(root.id, user, "go")

	collectFrames(t, root, func(seen []protocol.Frame) bool {
		if am, ok := seen[len(seen)-1].(*protocol.AgentMessage); ok && am.Payload.Final {
			return true
		}
		return false
	}, 5*time.Second)

	events, _ := store.ListEvents(context.Background(), root.id, ListEventsOpts{})
	soft := 0
	for _, ev := range events {
		if ev.EventType == string(protocol.KindSystemMessage) {
			if k, _ := ev.Metadata["kind"].(string); k == protocol.SystemMessageSoftWarning {
				soft++
			}
		}
	}
	if soft != 1 {
		t.Errorf("system_message{soft_warning} count = %d, want exactly 1; events=%v",
			soft, kindsOnly(events))
	}
}

// TestRoot_HardCeiling_TerminatesAtCapHard (§13.1 + §13.2 #8): cap_soft=2,
// cap_hard=4. The model wants 8 turns; the runtime terminates the
// session at iter=4 with reason "hard_ceiling", emits a
// hard_ceiling_hit marker, and writes the canonical
// session_terminated event.
func TestRoot_HardCeiling_TerminatesAtCapHard(t *testing.T) {
	const softCap = 2
	const hardCap = 4
	store := newFakeStore()
	mdl := newFanOutToolModel(8)
	provider := &stubProvider{
		tools:  []tool.Tool{{Name: "fake:do", Provider: "fake", PermissionObject: "hugen:tool:fake"}},
		result: `{"ok":true}`,
	}
	mgr := newCeilingTestManager(t, store, mdl, provider, softCap, hardCap)
	defer mgr.ShutdownAll(context.Background())

	root, _, err := mgr.Open(context.Background(), OpenRequest{OwnerID: "alice"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	drainOutboxOnce(root.Outbox())

	user := protocol.ParticipantInfo{ID: "u1", Kind: protocol.ParticipantUser}
	root.Inbox() <- protocol.NewUserMessage(root.id, user, "go")

	// Wait until the session is closed; ShutdownAll will drain the
	// outbox once the goroutine exits.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if root.IsClosed() {
			break
		}
		select {
		case _, ok := <-root.Outbox():
			if !ok {
				goto done
			}
		case <-time.After(20 * time.Millisecond):
		}
	}
done:

	events, _ := store.ListEvents(context.Background(), root.id, ListEventsOpts{})
	var sawCeilingMarker, sawTerminated bool
	for _, ev := range events {
		switch ev.EventType {
		case string(protocol.KindSystemMarker):
			if subj, _ := ev.Metadata["subject"].(string); subj == protocol.SubjectHardCeilingHit {
				sawCeilingMarker = true
			}
		case string(protocol.KindSessionTerminated):
			if reason, _ := ev.Metadata["reason"].(string); strings.Contains(reason, protocol.TerminationHardCeiling) {
				sawTerminated = true
			}
		}
	}
	if !sawCeilingMarker {
		t.Errorf("no hard_ceiling_hit system_marker in events; events=%v", kindsOnly(events))
	}
	if !sawTerminated {
		t.Errorf("no session_terminated{hard_ceiling}; events=%v", kindsOnly(events))
	}
}
