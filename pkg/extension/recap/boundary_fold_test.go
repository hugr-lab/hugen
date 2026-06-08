package recap

import (
	"context"
	"sync"
	"testing"

	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// stubModel returns a pre-canned reply on every Generate, counting calls.
// Mirrors the compactor test's stub (the summarizer model surface is the
// same Spec()+Generate() pair).
type stubModel struct {
	mu    sync.Mutex
	reply string
	calls int
}

func (m *stubModel) Spec() model.ModelSpec {
	return model.ModelSpec{Provider: "stub", Name: "recap-test"}
}

func (m *stubModel) Generate(_ context.Context, _ model.Request) (model.Stream, error) {
	m.mu.Lock()
	m.calls++
	content := m.reply
	m.mu.Unlock()
	return &oneChunkStream{content: &content}, nil
}

func (m *stubModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

type oneChunkStream struct {
	content *string
	sent    bool
}

func (s *oneChunkStream) Next(_ context.Context) (model.Chunk, bool, error) {
	if s.sent {
		return model.Chunk{}, false, nil
	}
	s.sent = true
	return model.Chunk{Content: s.content}, true, nil
}

func (s *oneChunkStream) Close() error { return nil }

func stubRouter(t *testing.T, m model.Model) *model.ModelRouter {
	t.Helper()
	r, err := model.NewModelRouter(
		map[model.Intent]model.ModelSpec{
			model.IntentDefault:   m.Spec(),
			model.IntentSummarize: m.Spec(),
		},
		map[model.ModelSpec]model.Model{m.Spec(): m},
	)
	if err != nil {
		t.Fatalf("NewModelRouter: %v", err)
	}
	return r
}

// TestOnTurnBoundary_FoldsSynchronously is the core db-2 freshness
// guarantee: OnTurnBoundary (re)forms the marker SYNCHRONOUSLY — by the
// time it returns, the marker is committed (so the turn that follows
// renders a current topic). A goroutine fold would race this read. Also
// asserts the fold does NOT run in OnFrameEmit.
func TestOnTurnBoundary_FoldsSynchronously(t *testing.T) {
	mdl := &stubModel{reply: `{"topic":"sales analysis","recap":"User asked to analyze quarterly sales by region.","keywords":["sales","regional"]}`}
	ext := NewExtension(Deps{Router: stubRouter(t, mdl)}, Config{})
	ctx := context.Background()
	state := fixture.NewTestSessionState("ses-root")
	if err := ext.InitState(ctx, state); err != nil {
		t.Fatalf("InitState: %v", err)
	}

	u := &protocol.UserMessage{Payload: protocol.UserMessagePayload{Text: "please analyze the quarterly sales data by region"}}
	ext.OnFrameEmit(ctx, state, u)

	// The fold must NOT have run yet — it moved out of OnFrameEmit.
	if mdl.callCount() != 0 {
		t.Fatalf("OnFrameEmit must not fold; model calls = %d", mdl.callCount())
	}

	// The boundary folds synchronously.
	if err := ext.OnTurnBoundary(ctx, state); err != nil {
		t.Fatalf("OnTurnBoundary: %v", err)
	}
	if mdl.callCount() != 1 {
		t.Fatalf("OnTurnBoundary should fold once; model calls = %d", mdl.callCount())
	}
	// Committed BEFORE OnTurnBoundary returned — read it right away.
	rec, ok := CurrentRecap(state)
	if !ok {
		t.Fatal("recap should exist after the fold")
	}
	if rec.Topic != "sales analysis" {
		t.Errorf("marker not committed synchronously; Topic = %q", rec.Topic)
	}
}

// TestOnTurnBoundary_NoOpWithoutNewUserMessage: with no trailing (new) user
// message in the ring, the fold short-circuits — the turn-start never
// blocks on the summarizer for nothing.
func TestOnTurnBoundary_NoOpWithoutNewUserMessage(t *testing.T) {
	mdl := &stubModel{reply: `{"topic":"x","recap":"y","keywords":[]}`}
	ext := NewExtension(Deps{Router: stubRouter(t, mdl)}, Config{})
	ctx := context.Background()
	state := fixture.NewTestSessionState("ses-root")
	_ = ext.InitState(ctx, state)

	// Only an assistant message — no new user input to (re)form a topic.
	reply := &protocol.AgentMessage{Payload: protocol.AgentMessagePayload{Text: "prior reply", Consolidated: true, Final: true}}
	ext.OnFrameEmit(ctx, state, reply)
	if err := ext.OnTurnBoundary(ctx, state); err != nil {
		t.Fatalf("OnTurnBoundary: %v", err)
	}
	if mdl.callCount() != 0 {
		t.Errorf("no new user message → no fold; model calls = %d", mdl.callCount())
	}
}

// TestOnTurnBoundary_NonRootNoOp: a non-root session has no recap handle,
// so the boundary hook is a no-op (no fold, no panic).
func TestOnTurnBoundary_NonRootNoOp(t *testing.T) {
	mdl := &stubModel{reply: `{"topic":"x","recap":"y","keywords":[]}`}
	ext := NewExtension(Deps{Router: stubRouter(t, mdl)}, Config{})
	ctx := context.Background()
	worker := fixture.NewTestSessionState("ses-w").WithDepth(1)
	_ = ext.InitState(ctx, worker) // no handle seeded for depth>0
	if err := ext.OnTurnBoundary(ctx, worker); err != nil {
		t.Fatalf("OnTurnBoundary non-root: %v", err)
	}
	if mdl.callCount() != 0 {
		t.Errorf("non-root must not fold; model calls = %d", mdl.callCount())
	}
}
