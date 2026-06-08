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

// lowFoldConfig makes any non-trivial tail cross the fold threshold
// (MaxRecapTokens=4 → ~16 chars → 0.75 → ~12-char trigger), so a single
// user message folds. RecapTargetTokens stays normal — the response cap is
// decoupled from the fold trigger.
func lowFoldConfig() Config { return Config{MaxRecapTokens: 4} }

// TestOnTurnBoundary_FoldsSynchronously is the core db-2 freshness
// guarantee: when the tail has crossed the threshold, OnTurnBoundary folds
// SYNCHRONOUSLY — by the time it returns, the compressed recap + topic are
// already committed (so the turn that follows renders a current recap).
// A goroutine fold would race this read.
func TestOnTurnBoundary_FoldsSynchronously(t *testing.T) {
	mdl := &stubModel{reply: `{"topic":"sales analysis","recap":"User asked to analyze quarterly sales by region.","keywords":["sales","regional"]}`}
	ext := NewExtension(Deps{Router: stubRouter(t, mdl)}, lowFoldConfig())
	ctx := context.Background()
	state := fixture.NewTestSessionState("ses-root")
	if err := ext.InitState(ctx, state); err != nil {
		t.Fatalf("InitState: %v", err)
	}

	// A user message long enough to cross the (tiny) fold threshold.
	u := &protocol.UserMessage{Payload: protocol.UserMessagePayload{Text: "please analyze the quarterly sales data by region"}}
	u.SetSeq(1)
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
		t.Errorf("compressed topic not committed synchronously; Topic = %q", rec.Topic)
	}
}

// TestOnTurnBoundary_NoOpBelowThreshold: a small tail does not fold, so the
// turn-start never blocks on the summarizer.
func TestOnTurnBoundary_NoOpBelowThreshold(t *testing.T) {
	mdl := &stubModel{reply: `{"topic":"x","recap":"y","keywords":[]}`}
	ext := NewExtension(Deps{Router: stubRouter(t, mdl)}, Config{}) // default 512-tok budget
	ctx := context.Background()
	state := fixture.NewTestSessionState("ses-root")
	_ = ext.InitState(ctx, state)

	u := &protocol.UserMessage{Payload: protocol.UserMessagePayload{Text: "hi"}}
	u.SetSeq(1)
	ext.OnFrameEmit(ctx, state, u)
	if err := ext.OnTurnBoundary(ctx, state); err != nil {
		t.Fatalf("OnTurnBoundary: %v", err)
	}
	if mdl.callCount() != 0 {
		t.Errorf("below-threshold turn must not fold; model calls = %d", mdl.callCount())
	}
	// The tail still carries the message verbatim (effective topic usable).
	rec, ok := CurrentRecap(state)
	if !ok || rec.Topic != "" {
		t.Errorf("below threshold: expected un-folded tail with empty topic; got %+v", rec)
	}
}

// TestOnTurnBoundary_NonRootNoOp: a non-root session has no recap handle,
// so the boundary hook is a no-op (no fold, no panic).
func TestOnTurnBoundary_NonRootNoOp(t *testing.T) {
	mdl := &stubModel{reply: `{"topic":"x","recap":"y","keywords":[]}`}
	ext := NewExtension(Deps{Router: stubRouter(t, mdl)}, lowFoldConfig())
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
