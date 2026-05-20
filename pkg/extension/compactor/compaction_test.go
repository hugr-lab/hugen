package compactor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/assets"
	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/prompts"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// β integration tests for the compaction pipeline. Each test
// builds an Extension manually wired against:
//
//   - a fakeIntegrationState that mirrors fakeState (extension_test.go)
//     but adds a real prompts.Renderer over assets.PromptsFS so
//     the summarise / collapse / block_c templates render, plus
//     a captured emit slice so the test can read the emitted
//     digest_set frames back.
//   - a fakeStoreReader returning canned EventRow rows for the
//     compactable range.
//   - a fakeModel + scriptedStream returning a pre-canned summary
//     (or an error in the hard-fallback path).
//
// The tests drive OnTurnBoundary, feed the FrameObserver path
// with synthetic user/agent frames, and assert the resulting
// DigestPayload shape.

// --- fixtures -----------------------------------------------

func productionRendererForCompactor(t *testing.T) *prompts.Renderer {
	t.Helper()
	sub, err := fs.Sub(assets.PromptsFS, "prompts")
	if err != nil {
		t.Fatalf("fs.Sub(assets.PromptsFS, prompts): %v", err)
	}
	return prompts.NewRenderer(sub, slog.Default())
}

// fakeIntegrationState extends fakeState with a real Prompts
// renderer + a captured Emit slice so the test can inspect the
// frames the extension persisted.
type fakeIntegrationState struct {
	id      string
	values  sync.Map
	prompts *prompts.Renderer
	mu      sync.Mutex
	emitted []protocol.Frame
}

func newFakeIntegrationState(t *testing.T, id string) *fakeIntegrationState {
	return &fakeIntegrationState{
		id:      id,
		prompts: productionRendererForCompactor(t),
	}
}

func (s *fakeIntegrationState) SessionID() string                      { return s.id }
func (s *fakeIntegrationState) SubagentName() string                   { return "" }
func (s *fakeIntegrationState) Role() string                           { return "" }
func (s *fakeIntegrationState) Skill() string                          { return "" }
func (s *fakeIntegrationState) Depth() int                             { return 0 }
func (s *fakeIntegrationState) Parent() (extension.SessionState, bool) { return nil, false }
func (s *fakeIntegrationState) Children() []extension.SessionState     { return nil }
func (s *fakeIntegrationState) Tools() *tool.ToolManager               { return nil }
func (s *fakeIntegrationState) Prompts() *prompts.Renderer             { return s.prompts }
func (s *fakeIntegrationState) Value(name string) (any, bool) {
	v, ok := s.values.Load(name)
	return v, ok
}
func (s *fakeIntegrationState) SetValue(name string, value any) { s.values.Store(name, value) }
func (s *fakeIntegrationState) Emit(_ context.Context, f protocol.Frame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emitted = append(s.emitted, f)
	return nil
}
func (s *fakeIntegrationState) IsClosed() bool { return false }
func (s *fakeIntegrationState) Submit(_ context.Context, _ protocol.Frame) <-chan struct{} {
	return nil
}
func (s *fakeIntegrationState) OutboxOnly(_ context.Context, _ protocol.Frame) error { return nil }
func (s *fakeIntegrationState) Extensions() []extension.Extension                    { return nil }
func (s *fakeIntegrationState) RequestInquiry(_ context.Context, _ protocol.InquiryRequestPayload) (*protocol.InquiryResponse, error) {
	return nil, nil
}

func (s *fakeIntegrationState) emittedFrames() []protocol.Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]protocol.Frame, len(s.emitted))
	copy(out, s.emitted)
	return out
}

// fakeStoreReader returns canned EventRow rows for the
// compactable range. ListEvents respects MinSeq + Limit; that's
// all the compaction pipeline needs.
type fakeStoreReader struct {
	rows []store.EventRow
}

func (r *fakeStoreReader) ListEvents(_ context.Context, _ string, opts store.ListEventsOpts) ([]store.EventRow, error) {
	out := make([]store.EventRow, 0, len(r.rows))
	for _, row := range r.rows {
		if opts.MinSeq > 0 && row.Seq <= opts.MinSeq {
			continue
		}
		out = append(out, row)
		if opts.Limit > 0 && len(out) >= opts.Limit {
			break
		}
	}
	return out, nil
}

// stubModel returns a pre-canned summary on every call. callCount
// + lastPromptTokens let tests assert call counts and inspect
// the rendered prompt.
type stubModel struct {
	mu       sync.Mutex
	summary  string
	err      error
	calls    int
	lastBody string
}

func (m *stubModel) Spec() model.ModelSpec {
	return model.ModelSpec{Provider: "stub", Name: "compactor-test"}
}

func (m *stubModel) Generate(_ context.Context, req model.Request) (model.Stream, error) {
	m.mu.Lock()
	m.calls++
	if len(req.Messages) > 0 {
		m.lastBody = req.Messages[0].Content
	}
	summary := m.summary
	err := m.err
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	content := summary
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

func newStubRouter(t *testing.T, m model.Model) *model.ModelRouter {
	t.Helper()
	defaults := map[model.Intent]model.ModelSpec{
		model.IntentDefault:   m.Spec(),
		model.IntentSummarize: m.Spec(),
	}
	models := map[model.ModelSpec]model.Model{m.Spec(): m}
	r, err := model.NewModelRouter(defaults, models)
	if err != nil {
		t.Fatalf("NewModelRouter: %v", err)
	}
	return r
}

// driveBoundaries simulates `count` completed user-turns by
// running the FrameObserver path with synthetic user_message +
// final consolidated agent_message pairs. Each turn occupies
// two persisted frames; seq starts at startSeq and advances by
// 2 per turn.
func driveBoundaries(t *testing.T, e *Extension, st extension.SessionState, count int, startSeq int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < count; i++ {
		userSeq := startSeq + i*2
		agentSeq := userSeq + 1
		um := protocol.NewUserMessage(st.SessionID(), protocol.ParticipantInfo{ID: "u1", Kind: protocol.ParticipantUser}, fmt.Sprintf("user msg #%d", i+1))
		um.SetSeq(userSeq)
		e.OnFrameEmit(ctx, st, um)
		ag := protocol.NewAgentMessage(st.SessionID(), protocol.ParticipantInfo{ID: "a1", Kind: protocol.ParticipantAgent}, fmt.Sprintf("agent reply #%d", i+1), 0, true)
		ag.Payload.Consolidated = true
		ag.SetSeq(agentSeq)
		e.OnFrameEmit(ctx, st, ag)
	}
}

// fixtureRows builds canned EventRow rows representing
// `count` user/agent turn pairs starting at startSeq. Mirrors
// the shape driveBoundaries produces on the FrameObserver path.
func fixtureRows(count int, startSeq int) []store.EventRow {
	rows := make([]store.EventRow, 0, count*2)
	for i := 0; i < count; i++ {
		userSeq := startSeq + i*2
		agentSeq := userSeq + 1
		rows = append(rows, store.EventRow{
			ID:        fmt.Sprintf("u%d", userSeq),
			Seq:       userSeq,
			EventType: string(protocol.KindUserMessage),
			Author:    "u1",
			Content:   fmt.Sprintf("user msg #%d", i+1),
			Metadata: map[string]any{
				"text": fmt.Sprintf("user msg #%d", i+1),
			},
		})
		rows = append(rows, store.EventRow{
			ID:        fmt.Sprintf("a%d", agentSeq),
			Seq:       agentSeq,
			EventType: string(protocol.KindAgentMessage),
			Author:    "a1",
			Content:   fmt.Sprintf("agent reply #%d", i+1),
			Metadata: map[string]any{
				"text":         fmt.Sprintf("agent reply #%d", i+1),
				"final":        true,
				"consolidated": true,
			},
		})
	}
	return rows
}

// extractDigest pulls the typed DigestPayload from the most-
// recent emitted ExtensionFrame, or returns nil + a t.Fatal when
// no digest was emitted.
func extractLatestDigest(t *testing.T, st *fakeIntegrationState) *DigestPayload {
	t.Helper()
	frames := st.emittedFrames()
	var latest *protocol.ExtensionFrame
	for _, f := range frames {
		ef, ok := f.(*protocol.ExtensionFrame)
		if !ok {
			continue
		}
		if ef.Payload.Extension != providerName || ef.Payload.Op != OpDigestSet {
			continue
		}
		latest = ef
	}
	if latest == nil {
		t.Fatalf("no digest_set frame emitted")
	}
	var d DigestPayload
	if err := json.Unmarshal(latest.Payload.Data, &d); err != nil {
		t.Fatalf("unmarshal digest payload: %v", err)
	}
	return &d
}

func countDigestSetFrames(st *fakeIntegrationState) int {
	frames := st.emittedFrames()
	n := 0
	for _, f := range frames {
		ef, ok := f.(*protocol.ExtensionFrame)
		if !ok {
			continue
		}
		if ef.Payload.Extension == providerName && ef.Payload.Op == OpDigestSet {
			n++
		}
	}
	return n
}

// --- tests --------------------------------------------------

// TestCompactor_Smoke fires one compaction on a session with
// 60 completed user-turns when MaxTurns=50 + PreservedRecentTurns=10.
// Exactly one digest_set lands; the digest carries one
// SummaryBlock and 60-ish KeptVerbatim user/agent entries.
func TestCompactor_Smoke(t *testing.T) {
	const turns = 60
	startSeq := 1
	st := newFakeIntegrationState(t, "ses-smoke")
	model := &stubModel{summary: "- tools were called; user asked Q; agent answered"}
	router := newStubRouter(t, model)
	storeR := &fakeStoreReader{rows: fixtureRows(turns, startSeq)}

	cfg := DefaultConfig()
	cfg.MaxTurns = 50
	cfg.PreservedRecentTurns = 10
	cfg.MinTurnGap = 3
	cfg.DigestMaxTokens = 0 // disable cap-collapse for this test

	e := NewExtension(slog.Default(), cfg, Deps{
		Router:  router,
		Store:   storeR,
		AgentID: "a1",
	})
	if err := e.InitState(context.Background(), st); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	driveBoundaries(t, e, st, turns, startSeq)

	if err := e.OnTurnBoundary(context.Background(), st); err != nil {
		t.Fatalf("OnTurnBoundary: %v", err)
	}
	if got := model.callCount(); got != 1 {
		t.Fatalf("model called %d times, want 1", got)
	}
	if got := countDigestSetFrames(st); got != 1 {
		t.Fatalf("digest_set frames emitted = %d, want 1", got)
	}
	d := extractLatestDigest(t, st)
	if d.Iteration != 1 {
		t.Fatalf("digest iteration = %d, want 1", d.Iteration)
	}
	if len(d.SummaryBlocks) != 1 {
		t.Fatalf("summary blocks = %d, want 1", len(d.SummaryBlocks))
	}
	if !strings.Contains(d.SummaryBlocks[0].Text, "user asked") {
		t.Fatalf("summary block text = %q, want stub summary", d.SummaryBlocks[0].Text)
	}
	if d.CutoffSeq <= 0 {
		t.Fatalf("digest cutoff seq = %d, want > 0", d.CutoffSeq)
	}
	// In-memory projection should mirror what was persisted.
	if got := FromState(st).Digest(); got == nil || got.Iteration != 1 {
		t.Fatalf("in-memory digest = %+v, want iteration 1", got)
	}
}

// TestCompactor_MultiIteration fires two compactions on a long
// session: 110 turns with MinTurnGap=3 produces ≥2 digest_set
// emissions and the latest digest has 2 SummaryBlocks.
func TestCompactor_MultiIteration(t *testing.T) {
	const turns = 110
	startSeq := 1
	st := newFakeIntegrationState(t, "ses-multi")
	mdl := &stubModel{summary: "- iteration summary"}
	router := newStubRouter(t, mdl)
	storeR := &fakeStoreReader{rows: fixtureRows(turns, startSeq)}

	cfg := DefaultConfig()
	cfg.MaxTurns = 50
	cfg.PreservedRecentTurns = 10
	cfg.MinTurnGap = 3
	cfg.DigestMaxTokens = 0 // keep blocks separate

	e := NewExtension(slog.Default(), cfg, Deps{
		Router:  router,
		Store:   storeR,
		AgentID: "a1",
	})
	if err := e.InitState(context.Background(), st); err != nil {
		t.Fatalf("InitState: %v", err)
	}

	// First batch: 60 turns → first compaction.
	driveBoundaries(t, e, st, 60, startSeq)
	if err := e.OnTurnBoundary(context.Background(), st); err != nil {
		t.Fatalf("OnTurnBoundary #1: %v", err)
	}
	if got := countDigestSetFrames(st); got != 1 {
		t.Fatalf("after first batch: digest_set frames = %d, want 1", got)
	}
	// Second batch: 50 more turns (total 110).
	driveBoundaries(t, e, st, 50, startSeq+60*2)
	if err := e.OnTurnBoundary(context.Background(), st); err != nil {
		t.Fatalf("OnTurnBoundary #2: %v", err)
	}
	if got := countDigestSetFrames(st); got < 2 {
		t.Fatalf("after second batch: digest_set frames = %d, want ≥2", got)
	}
	d := extractLatestDigest(t, st)
	if d.Iteration < 2 {
		t.Fatalf("latest digest iteration = %d, want ≥2", d.Iteration)
	}
	if len(d.SummaryBlocks) != d.Iteration {
		t.Fatalf("summary blocks = %d, want %d (one per iteration)", len(d.SummaryBlocks), d.Iteration)
	}
}

// TestCompactor_CapCollapse forces the cap-driven collapse by
// setting an unreasonably low DigestMaxTokens. After two
// compactions, the latest digest should have exactly one
// SummaryBlock (collapsed).
func TestCompactor_CapCollapse(t *testing.T) {
	const turns = 110
	startSeq := 1
	st := newFakeIntegrationState(t, "ses-cap")
	mdl := &stubModel{summary: strings.Repeat("- iteration summary ", 50)}
	router := newStubRouter(t, mdl)
	storeR := &fakeStoreReader{rows: fixtureRows(turns, startSeq)}

	cfg := DefaultConfig()
	cfg.MaxTurns = 50
	cfg.PreservedRecentTurns = 10
	cfg.MinTurnGap = 3
	cfg.DigestMaxTokens = 50 // very low to force collapse on second fire

	e := NewExtension(slog.Default(), cfg, Deps{
		Router:  router,
		Store:   storeR,
		AgentID: "a1",
	})
	if err := e.InitState(context.Background(), st); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	driveBoundaries(t, e, st, 60, startSeq)
	if err := e.OnTurnBoundary(context.Background(), st); err != nil {
		t.Fatalf("OnTurnBoundary #1: %v", err)
	}
	driveBoundaries(t, e, st, 50, startSeq+60*2)
	if err := e.OnTurnBoundary(context.Background(), st); err != nil {
		t.Fatalf("OnTurnBoundary #2: %v", err)
	}
	d := extractLatestDigest(t, st)
	if len(d.SummaryBlocks) != 1 {
		t.Fatalf("after cap-collapse: summary blocks = %d, want 1", len(d.SummaryBlocks))
	}
	if d.SummaryBlocks[0].To != d.CutoffSeq {
		t.Fatalf("collapsed block To = %d, want %d (= CutoffSeq)", d.SummaryBlocks[0].To, d.CutoffSeq)
	}
}

// TestCompactor_HardFallback exercises the marker-block fallback
// (spec §5.6): when the LLM call errors, the extension still
// emits digest_set with a "(LLM summary failed: …)" marker block
// and a populated KeptVerbatim.
func TestCompactor_HardFallback(t *testing.T) {
	const turns = 60
	startSeq := 1
	st := newFakeIntegrationState(t, "ses-fb")
	mdl := &stubModel{err: errors.New("model unavailable")}
	router := newStubRouter(t, mdl)
	storeR := &fakeStoreReader{rows: fixtureRows(turns, startSeq)}

	cfg := DefaultConfig()
	cfg.MaxTurns = 50
	cfg.PreservedRecentTurns = 10
	cfg.MinTurnGap = 3
	cfg.DigestMaxTokens = 0

	e := NewExtension(slog.Default(), cfg, Deps{
		Router:  router,
		Store:   storeR,
		AgentID: "a1",
	})
	if err := e.InitState(context.Background(), st); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	driveBoundaries(t, e, st, turns, startSeq)
	if err := e.OnTurnBoundary(context.Background(), st); err != nil {
		t.Fatalf("OnTurnBoundary: %v", err)
	}
	if got := countDigestSetFrames(st); got != 1 {
		t.Fatalf("digest_set frames = %d, want 1", got)
	}
	d := extractLatestDigest(t, st)
	if len(d.SummaryBlocks) != 1 {
		t.Fatalf("summary blocks = %d, want 1", len(d.SummaryBlocks))
	}
	if !strings.Contains(d.SummaryBlocks[0].Text, "LLM summary failed") {
		t.Fatalf("fallback marker text = %q, want LLM-summary-failed marker", d.SummaryBlocks[0].Text)
	}
	if len(d.KeptVerbatim) == 0 {
		t.Fatalf("KeptVerbatim empty on fallback path; want populated")
	}
}

// TestCompactor_MinTurnGap blocks the second compaction when not
// enough completed turns have elapsed since the first fire.
func TestCompactor_MinTurnGap(t *testing.T) {
	const turns = 60
	startSeq := 1
	st := newFakeIntegrationState(t, "ses-gap")
	mdl := &stubModel{summary: "- summary"}
	router := newStubRouter(t, mdl)
	storeR := &fakeStoreReader{rows: fixtureRows(turns, startSeq)}

	cfg := DefaultConfig()
	cfg.MaxTurns = 50
	cfg.PreservedRecentTurns = 10
	cfg.MinTurnGap = 100 // wildly large gap; second fire must block
	cfg.DigestMaxTokens = 0

	e := NewExtension(slog.Default(), cfg, Deps{
		Router:  router,
		Store:   storeR,
		AgentID: "a1",
	})
	if err := e.InitState(context.Background(), st); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	driveBoundaries(t, e, st, 60, startSeq)
	if err := e.OnTurnBoundary(context.Background(), st); err != nil {
		t.Fatalf("OnTurnBoundary #1: %v", err)
	}
	// Add a couple more turns — should not advance past MinTurnGap.
	driveBoundaries(t, e, st, 2, startSeq+60*2)
	if err := e.OnTurnBoundary(context.Background(), st); err != nil {
		t.Fatalf("OnTurnBoundary #2: %v", err)
	}
	if got := countDigestSetFrames(st); got != 1 {
		t.Fatalf("digest_set frames = %d, want exactly 1 (gap should block)", got)
	}
}

// TestAdvertise_RendersBlockC after a successful compaction, the
// Advertiser surface returns a non-empty Block C body that
// references the cutoff seq.
func TestAdvertise_RendersBlockC(t *testing.T) {
	const turns = 60
	startSeq := 1
	st := newFakeIntegrationState(t, "ses-adv")
	mdl := &stubModel{summary: "- summary body"}
	router := newStubRouter(t, mdl)
	storeR := &fakeStoreReader{rows: fixtureRows(turns, startSeq)}

	cfg := DefaultConfig()
	cfg.MaxTurns = 50
	cfg.PreservedRecentTurns = 10
	cfg.MinTurnGap = 3
	cfg.DigestMaxTokens = 0

	e := NewExtension(slog.Default(), cfg, Deps{
		Router:  router,
		Store:   storeR,
		AgentID: "a1",
	})
	if err := e.InitState(context.Background(), st); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	driveBoundaries(t, e, st, turns, startSeq)
	if err := e.OnTurnBoundary(context.Background(), st); err != nil {
		t.Fatalf("OnTurnBoundary: %v", err)
	}

	out := e.AdvertiseSystemPrompt(context.Background(), st)
	if out == "" {
		t.Fatalf("AdvertiseSystemPrompt returned empty after successful compaction")
	}
	if !strings.Contains(out, "History digest") {
		t.Fatalf("Block C body missing header marker; got:\n%s", out)
	}
	if !strings.Contains(out, "summary body") {
		t.Fatalf("Block C body missing summariser text; got:\n%s", out)
	}
}

// TestClassifyRows_PerKindDispatch verifies the §5.2 table —
// every Kind is binned correctly: user/agent → kept,
// tool_call+result → toolPairs, inquiry pair → inquiries,
// subagent_result → refs, drops → dropped.
func TestClassifyRows_PerKindDispatch(t *testing.T) {
	rows := []store.EventRow{
		{
			Seq:       1,
			EventType: string(protocol.KindUserMessage),
			Author:    "u1",
			Content:   "hello",
		},
		{
			Seq:       2,
			EventType: string(protocol.KindAgentMessage),
			Author:    "a1",
			Content:   "world",
			Metadata: map[string]any{
				"final":        true,
				"consolidated": true,
			},
		},
		// streaming agent_message — must be dropped
		{
			Seq:       3,
			EventType: string(protocol.KindAgentMessage),
			Author:    "a1",
			Content:   "streaming",
			Metadata: map[string]any{
				"final":        false,
				"consolidated": false,
			},
		},
		{
			Seq:       4,
			EventType: string(protocol.KindToolCall),
			Author:    "a1",
			ToolName:  "test-tool",
			ToolArgs:  map[string]any{"q": "v"},
			Metadata:  map[string]any{"tool_id": "tc-1"},
		},
		{
			Seq:        5,
			EventType:  string(protocol.KindToolResult),
			Author:     "a1",
			ToolResult: "ok",
			Metadata:   map[string]any{"tool_id": "tc-1"},
		},
		{
			Seq:       6,
			EventType: string(protocol.KindSubagentResult),
			Author:    "a1",
			Content:   "child done",
			Metadata: map[string]any{
				"session_id": "child-1",
				"reason":     "completed",
			},
		},
		{
			Seq:       7,
			EventType: string(protocol.KindReasoning),
			Author:    "a1",
			Content:   "internal thought",
		},
		{
			Seq:       8,
			EventType: string(protocol.KindError),
			Author:    "a1",
			Content:   "terminal boom",
			Metadata: map[string]any{
				"recoverable": false,
			},
		},
	}
	c := classifyRows(rows)
	// kept: user_message + final agent_message + terminal error
	if len(c.kept) != 3 {
		t.Fatalf("kept = %d entries, want 3", len(c.kept))
	}
	if len(c.toolPairs) != 1 {
		t.Fatalf("toolPairs = %d, want 1", len(c.toolPairs))
	}
	if c.toolPairs[0].ToolName != "test-tool" || c.toolPairs[0].Result != "ok" {
		t.Fatalf("toolPair mismatched: %+v", c.toolPairs[0])
	}
	if len(c.refs) != 1 {
		t.Fatalf("subagent refs = %d, want 1", len(c.refs))
	}
	if c.refs[0].SessionID != "child-1" || c.refs[0].Reason != "completed" {
		t.Fatalf("subagent ref mismatched: %+v", c.refs[0])
	}
}

// TestEstimateDigestTokens grows monotonically with payload size.
func TestEstimateDigestTokens(t *testing.T) {
	d := &DigestPayload{}
	zero := estimateDigestTokens(d)
	d.SummaryBlocks = []SummaryBlock{{Text: strings.Repeat("a", 100)}}
	one := estimateDigestTokens(d)
	d.SummaryBlocks = append(d.SummaryBlocks, SummaryBlock{Text: strings.Repeat("a", 100)})
	two := estimateDigestTokens(d)
	if !(zero == 0 && one > zero && two > one) {
		t.Fatalf("estimateDigestTokens monotonicity broken: 0=%d 1=%d 2=%d", zero, one, two)
	}
}

// TestDedupRefs collapses by SessionID + keeps last reason.
func TestDedupRefs(t *testing.T) {
	in := []SubagentRef{
		{SessionID: "a", Reason: "first"},
		{SessionID: "b", Reason: "ok"},
		{SessionID: "a", Reason: "latest"},
	}
	out := dedupRefs(in)
	if len(out) != 2 {
		t.Fatalf("dedup count = %d, want 2", len(out))
	}
	for _, r := range out {
		if r.SessionID == "a" && r.Reason != "latest" {
			t.Fatalf("expected last-write-wins on session a; got reason=%q", r.Reason)
		}
	}
}

// TestStreamModelText_Timeout asserts the context-deadline path
// returns an error promptly (defends against the LLMTimeout
// config knob silently being ignored).
func TestStreamModelText_Timeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	mdl := &blockingModel{}
	_, err := streamModelText(ctx, mdl, "body", 100)
	if err == nil {
		t.Fatalf("streamModelText should error on cancelled ctx")
	}
}

// blockingModel.Generate blocks forever — used to defend the
// deadline path in streamModelText.
type blockingModel struct{}

func (blockingModel) Spec() model.ModelSpec { return model.ModelSpec{Provider: "stub", Name: "block"} }
func (blockingModel) Generate(ctx context.Context, _ model.Request) (model.Stream, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
