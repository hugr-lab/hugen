package compactor

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/prompts"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// fakeState is the minimal extension.SessionState the compactor
// tests need: a value bag + getters that return zero values. Mirrors
// the pattern used by mission's executor_test.go.
type fakeState struct {
	id     string
	values sync.Map
}

func newFakeState(id string) *fakeState { return &fakeState{id: id} }

func (s *fakeState) SessionID() string                              { return s.id }
func (s *fakeState) SubagentName() string                           { return "" }
func (s *fakeState) Role() string                                   { return "" }
func (s *fakeState) Skill() string                                  { return "" }
func (s *fakeState) Depth() int                                     { return 0 }
func (s *fakeState) Parent() (extension.SessionState, bool)         { return nil, false }
func (s *fakeState) Children() []extension.SessionState             { return nil }
func (s *fakeState) Tools() *tool.ToolManager                       { return nil }
func (s *fakeState) Prompts() *prompts.Renderer                     { return nil }
func (s *fakeState) Value(name string) (any, bool)                  { v, ok := s.values.Load(name); return v, ok }
func (s *fakeState) SetValue(name string, value any)                { s.values.Store(name, value) }
func (s *fakeState) Emit(_ context.Context, _ protocol.Frame) error { return nil }
func (s *fakeState) IsClosed() bool                                 { return false }
func (s *fakeState) Submit(_ context.Context, _ protocol.Frame) <-chan struct{} {
	return nil
}
func (s *fakeState) OutboxOnly(_ context.Context, _ protocol.Frame) error { return nil }
func (s *fakeState) Extensions() []extension.Extension                    { return nil }
func (s *fakeState) RequestInquiry(_ context.Context, _ protocol.InquiryRequestPayload) (*protocol.InquiryResponse, error) {
	return nil, nil
}

func newTestExtension(t *testing.T) *Extension {
	t.Helper()
	return NewExtension(slog.Default(), DefaultConfig(), Deps{})
}

// --- state.go ---

func TestCompactorState_SetGetClear(t *testing.T) {
	s := &CompactorState{}
	if s.Digest() != nil {
		t.Fatalf("fresh state should have nil digest")
	}
	d := &DigestPayload{Version: 1, Iteration: 1, CutoffSeq: 42}
	s.SetDigest(d)
	got := s.Digest()
	if got == nil || got.Iteration != 1 || got.CutoffSeq != 42 {
		t.Fatalf("Digest() after Set returned %+v", got)
	}
	s.ClearDigest()
	if s.Digest() != nil {
		t.Fatalf("Digest() after Clear should be nil")
	}
}

func TestCompactorState_BoundaryAppendAndCount(t *testing.T) {
	s := &CompactorState{}
	if got := s.BoundaryCount(); got != 0 {
		t.Fatalf("initial boundary count = %d, want 0", got)
	}
	s.appendBoundary(10, 25)
	s.appendBoundary(20, 50)
	s.appendBoundary(30, 100)
	if got := s.BoundaryCount(); got != 3 {
		t.Fatalf("boundary count = %d, want 3", got)
	}
	if got := s.BoundaryAt(0); got != 10 {
		t.Fatalf("BoundaryAt(0) = %d, want 10", got)
	}
	if got := s.BoundaryAt(2); got != 30 {
		t.Fatalf("BoundaryAt(2) = %d, want 30", got)
	}
	if got := s.EstimatedPromptTokens(); got != 175 {
		t.Fatalf("estimated tokens = %d, want 175", got)
	}
}

func TestCompactorState_ZeroSeqSkipsBoundary(t *testing.T) {
	// appendBoundary(0, …) should bump tokens but NOT record a
	// boundary entry — the runtime guarantees persisted frames
	// have non-zero seqs; a zero is treated as "not yet
	// persisted" and skipped to keep the boundary list clean.
	s := &CompactorState{}
	s.appendBoundary(0, 33)
	if got := s.BoundaryCount(); got != 0 {
		t.Fatalf("zero-seq should not record boundary; got count = %d", got)
	}
	if got := s.EstimatedPromptTokens(); got != 33 {
		t.Fatalf("zero-seq should still contribute tokens; got = %d", got)
	}
}

// --- extension.go ---

func TestExtension_InitStateAttachesHandle(t *testing.T) {
	e := newTestExtension(t)
	st := newFakeState("ses-1")
	if err := e.InitState(context.Background(), st); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	if got := FromState(st); got == nil {
		t.Fatalf("FromState returned nil after InitState")
	}
}

func TestExtension_FromStateNil(t *testing.T) {
	if got := FromState(nil); got != nil {
		t.Fatalf("FromState(nil) = %+v, want nil", got)
	}
	st := newFakeState("ses-2") // no InitState fired
	if got := FromState(st); got != nil {
		t.Fatalf("FromState without InitState = %+v, want nil", got)
	}
}

func TestExtension_NameAndAdvertiseEmptyByDefault(t *testing.T) {
	e := newTestExtension(t)
	if e.Name() != providerName {
		t.Fatalf("Name() = %q, want %q", e.Name(), providerName)
	}
	st := newFakeState("ses-3")
	if err := e.InitState(context.Background(), st); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	if got := e.AdvertiseSystemPrompt(context.Background(), st); got != "" {
		t.Fatalf("AdvertiseSystemPrompt with empty digest = %q, want empty", got)
	}
}

// --- trigger.go ---

func TestExtension_OnTurnBoundary_NoDepsSkips(t *testing.T) {
	// β: with no Router / Store wired the trigger predicate
	// short-circuits — OnTurnBoundary is a no-op that always
	// returns nil. Mirrors the test-fixture / α-style boot path.
	e := newTestExtension(t)
	st := newFakeState("ses-4")
	if err := e.InitState(context.Background(), st); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	if err := e.OnTurnBoundary(context.Background(), st); err != nil {
		t.Fatalf("OnTurnBoundary returned %v, want nil", err)
	}
}

func TestExtension_OnTurnBoundary_DisabledShortCircuits(t *testing.T) {
	e := NewExtension(slog.Default(), Config{Enabled: false}, Deps{})
	st := newFakeState("ses-5")
	if err := e.InitState(context.Background(), st); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	if err := e.OnTurnBoundary(context.Background(), st); err != nil {
		t.Fatalf("disabled hook returned %v, want nil", err)
	}
}

// --- frame_observer.go ---

func TestExtension_OnFrameEmit_UserMessageRecordsBoundary(t *testing.T) {
	e := newTestExtension(t)
	st := newFakeState("ses-6")
	if err := e.InitState(context.Background(), st); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	user := protocol.NewUserMessage(st.id, protocol.ParticipantInfo{}, "tell me about Q3 sales")
	user.SetSeq(7)
	e.OnFrameEmit(context.Background(), st, user)
	s := FromState(st)
	if got := s.BoundaryCount(); got != 1 {
		t.Fatalf("boundary count after user_message = %d, want 1", got)
	}
	if got := s.BoundaryAt(0); got != 7 {
		t.Fatalf("BoundaryAt(0) = %d, want 7", got)
	}
	if s.EstimatedPromptTokens() == 0 {
		t.Fatalf("user_message should contribute tokens; got 0")
	}
}

func TestExtension_OnFrameEmit_FinalAgentMessageContributesTokens(t *testing.T) {
	e := newTestExtension(t)
	st := newFakeState("ses-7")
	if err := e.InitState(context.Background(), st); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	final := protocol.NewAgentMessage(st.id, protocol.ParticipantInfo{}, "final answer text", 0, true)
	final.Payload.Consolidated = true
	final.SetSeq(12)
	e.OnFrameEmit(context.Background(), st, final)
	s := FromState(st)
	if got := s.BoundaryCount(); got != 0 {
		t.Fatalf("agent_message should not record a boundary; got %d", got)
	}
	if s.EstimatedPromptTokens() == 0 {
		t.Fatalf("agent_message should contribute tokens")
	}
}

func TestExtension_OnFrameEmit_StreamingAgentMessageIgnored(t *testing.T) {
	// Non-consolidated chunks are outbox-only and shouldn't
	// inflate the token estimate (they never persist).
	e := newTestExtension(t)
	st := newFakeState("ses-8")
	if err := e.InitState(context.Background(), st); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	chunk := protocol.NewAgentMessage(st.id, protocol.ParticipantInfo{}, "streaming chunk", 0, false)
	chunk.SetSeq(13)
	e.OnFrameEmit(context.Background(), st, chunk)
	s := FromState(st)
	if got := s.EstimatedPromptTokens(); got != 0 {
		t.Fatalf("streaming chunk should not contribute tokens; got %d", got)
	}
}

// --- recovery.go ---

func TestRecover_LatestDigestSetWins(t *testing.T) {
	e := newTestExtension(t)
	st := newFakeState("ses-9")
	if err := e.InitState(context.Background(), st); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	events := []store.EventRow{
		mkDigestSetRow(t, 1, &DigestPayload{Version: 1, Iteration: 1, CutoffSeq: 10}),
		mkDigestSetRow(t, 2, &DigestPayload{Version: 1, Iteration: 2, CutoffSeq: 50}),
		mkDigestSetRow(t, 3, &DigestPayload{Version: 1, Iteration: 3, CutoffSeq: 100}),
	}
	if err := e.Recover(context.Background(), st, events); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	got := FromState(st).Digest()
	if got == nil {
		t.Fatalf("Recover should have set a digest")
	}
	if got.Iteration != 3 || got.CutoffSeq != 100 {
		t.Fatalf("latest digest = iter %d seq %d, want iter 3 seq 100", got.Iteration, got.CutoffSeq)
	}
}

func TestRecover_DigestClearWipesState(t *testing.T) {
	e := newTestExtension(t)
	st := newFakeState("ses-10")
	if err := e.InitState(context.Background(), st); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	events := []store.EventRow{
		mkDigestSetRow(t, 1, &DigestPayload{Version: 1, Iteration: 1, CutoffSeq: 10}),
		mkDigestClearRow(t, 2),
	}
	if err := e.Recover(context.Background(), st, events); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := FromState(st).Digest(); got != nil {
		t.Fatalf("digest_clear should wipe; got %+v", got)
	}
}

func TestRecover_FutureVersionSkipped(t *testing.T) {
	e := newTestExtension(t)
	st := newFakeState("ses-11")
	if err := e.InitState(context.Background(), st); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	events := []store.EventRow{
		mkDigestSetRow(t, 1, &DigestPayload{Version: 1, Iteration: 1, CutoffSeq: 10}),
		mkDigestSetRow(t, 2, &DigestPayload{Version: CurrentPayloadVersion + 5, Iteration: 99, CutoffSeq: 999}),
	}
	if err := e.Recover(context.Background(), st, events); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	got := FromState(st).Digest()
	if got == nil || got.Iteration != 1 {
		t.Fatalf("future-version row should be skipped; got %+v", got)
	}
}

func TestRecover_UnknownOpSkipped(t *testing.T) {
	e := newTestExtension(t)
	st := newFakeState("ses-12")
	if err := e.InitState(context.Background(), st); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	events := []store.EventRow{
		mkDigestSetRow(t, 1, &DigestPayload{Version: 1, Iteration: 7, CutoffSeq: 70}),
		{
			EventType: string(protocol.KindExtensionFrame),
			Seq:       2,
			Metadata: map[string]any{
				"extension": providerName,
				"op":        "wat_is_this",
				"data":      map[string]any{"version": 1},
			},
		},
	}
	if err := e.Recover(context.Background(), st, events); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	got := FromState(st).Digest()
	if got == nil || got.Iteration != 7 {
		t.Fatalf("unknown op should not alter prior state; got %+v", got)
	}
}

func TestRecover_OtherExtensionRowsIgnored(t *testing.T) {
	e := newTestExtension(t)
	st := newFakeState("ses-13")
	if err := e.InitState(context.Background(), st); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	events := []store.EventRow{
		// row from a different extension — must be ignored
		{
			EventType: string(protocol.KindExtensionFrame),
			Seq:       1,
			Metadata: map[string]any{
				"extension": "plan",
				"op":        "set",
				"data":      map[string]any{"text": "step 1"},
			},
		},
		// our row
		mkDigestSetRow(t, 2, &DigestPayload{Version: 1, Iteration: 1, CutoffSeq: 5}),
	}
	if err := e.Recover(context.Background(), st, events); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	got := FromState(st).Digest()
	if got == nil || got.Iteration != 1 {
		t.Fatalf("other-extension rows should be ignored; got %+v", got)
	}
}

// mkDigestSetRow builds an EventRow simulating a persisted
// digest_set ExtensionFrame the way the runtime would store it.
func mkDigestSetRow(t *testing.T, seq int, d *DigestPayload) store.EventRow {
	t.Helper()
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal digest: %v", err)
	}
	// Decode back to map so Metadata carries the raw shape the
	// store materialises into.
	var data any
	if err := json.Unmarshal(b, &data); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	return store.EventRow{
		ID:        "evt-" + d.BuiltAt.String(),
		Seq:       seq,
		EventType: string(protocol.KindExtensionFrame),
		Metadata: map[string]any{
			"extension": providerName,
			"category":  "op",
			"op":        OpDigestSet,
			"data":      data,
		},
		CreatedAt: time.Now(),
	}
}

func mkDigestClearRow(t *testing.T, seq int) store.EventRow {
	t.Helper()
	return store.EventRow{
		ID:        "evt-clear",
		Seq:       seq,
		EventType: string(protocol.KindExtensionFrame),
		Metadata: map[string]any{
			"extension": providerName,
			"category":  "op",
			"op":        OpDigestClear,
		},
		CreatedAt: time.Now(),
	}
}
