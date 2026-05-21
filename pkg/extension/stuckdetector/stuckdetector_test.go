package stuckdetector

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/assets"
	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/prompts"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// fakeState is the minimal [extension.SessionState] the
// detector tests need: a value bag, the production prompts
// renderer (so MustRender works), and a captured emit slice so
// rising-edge tests can sanity-check that the nudge frame
// landed.
type fakeState struct {
	id      string
	values  sync.Map
	prompts *prompts.Renderer
	mu      sync.Mutex
	emitted []protocol.Frame
}

func newFakeState(t *testing.T, id string) *fakeState {
	t.Helper()
	sub, err := fs.Sub(assets.PromptsFS, "prompts")
	if err != nil {
		t.Fatalf("fs.Sub(assets.PromptsFS, prompts): %v", err)
	}
	return &fakeState{
		id:      id,
		prompts: prompts.NewRenderer(sub, slog.New(slog.NewTextHandler(io.Discard, nil))),
	}
}

func (s *fakeState) SessionID() string                      { return s.id }
func (s *fakeState) SubagentName() string                   { return "" }
func (s *fakeState) Role() string                           { return "" }
func (s *fakeState) Skill() string                          { return "" }
func (s *fakeState) Depth() int                             { return 0 }
func (s *fakeState) Parent() (extension.SessionState, bool) { return nil, false }
func (s *fakeState) Children() []extension.SessionState     { return nil }
func (s *fakeState) Tools() *tool.ToolManager               { return nil }
func (s *fakeState) Prompts() *prompts.Renderer             { return s.prompts }
func (s *fakeState) Value(name string) (any, bool) {
	v, ok := s.values.Load(name)
	return v, ok
}
func (s *fakeState) SetValue(name string, value any) { s.values.Store(name, value) }
func (s *fakeState) Emit(_ context.Context, f protocol.Frame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emitted = append(s.emitted, f)
	return nil
}
func (s *fakeState) IsClosed() bool { return false }
func (s *fakeState) Submit(_ context.Context, _ protocol.Frame) <-chan struct{} {
	return nil
}
func (s *fakeState) OutboxOnly(_ context.Context, _ protocol.Frame) error { return nil }
func (s *fakeState) ToolCatalogTokens(_ context.Context) int               { return 0 }
func (s *fakeState) SessionUsage() *protocol.TokenUsage                    { return nil }
func (s *fakeState) Extensions() []extension.Extension                    { return nil }
func (s *fakeState) RequestInquiry(_ context.Context, _ protocol.InquiryRequestPayload) (*protocol.InquiryResponse, error) {
	return nil, nil
}

func (s *fakeState) emittedFrames() []protocol.Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]protocol.Frame, len(s.emitted))
	copy(out, s.emitted)
	return out
}

func newTestExtension(t *testing.T) *Extension {
	t.Helper()
	return NewExtension(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// observeCall stuffs a consolidated AgentMessage with the given
// tool calls into [Extension.OnFrameEmit], driving the detector
// the same way the runtime would on a real session.
func observeCall(t *testing.T, e *Extension, st *fakeState, at time.Time, tcs ...protocol.ToolCallPayload) {
	t.Helper()
	frame := &protocol.AgentMessage{
		BaseFrame: protocol.BaseFrame{
			Session: st.id,
			K:       protocol.KindAgentMessage,
			At:      at,
		},
		Payload: protocol.AgentMessagePayload{
			Consolidated: true,
			ToolCalls:    tcs,
		},
	}
	e.OnFrameEmit(context.Background(), st, frame)
}

// observeResult feeds a ToolResult frame matching the given
// tool_id with errCode in the result envelope.
func observeResult(t *testing.T, e *Extension, st *fakeState, toolID, errCode string) {
	t.Helper()
	var (
		result  any
		isError bool
	)
	if errCode != "" {
		isError = true
		result = protocol.ToolError{Code: errCode}
	}
	frame := &protocol.ToolResult{
		BaseFrame: protocol.BaseFrame{
			Session: st.id,
			K:       protocol.KindToolResult,
		},
		Payload: protocol.ToolResultPayload{
			ToolID:  toolID,
			Result:  result,
			IsError: isError,
		},
	}
	e.OnFrameEmit(context.Background(), st, frame)
}

func TestStuckDetector_RepeatedHashRisingEdge(t *testing.T) {
	e := newTestExtension(t)
	st := newFakeState(t, "ses-h1")
	if err := e.InitState(context.Background(), st); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	now := time.Now()
	args := map[string]any{"x": 1}

	// Two identical calls — not enough for a rising edge.
	observeCall(t, e, st, now, protocol.ToolCallPayload{ToolID: "tc-1", Name: "fake:do", Args: args})
	observeCall(t, e, st, now.Add(50*time.Millisecond), protocol.ToolCallPayload{ToolID: "tc-2", Name: "fake:do", Args: args})
	s := FromState(st)
	if s.repeatedHashActive {
		t.Fatalf("active after 2 identical calls; need 3")
	}

	// Third identical call — rising edge.
	observeCall(t, e, st, now.Add(100*time.Millisecond), protocol.ToolCallPayload{ToolID: "tc-3", Name: "fake:do", Args: args})
	if !s.repeatedHashActive {
		t.Fatalf("rising edge missed on third identical call")
	}

	// Pattern break.
	otherArgs := map[string]any{"x": 2}
	observeCall(t, e, st, now.Add(150*time.Millisecond), protocol.ToolCallPayload{ToolID: "tc-4", Name: "fake:do", Args: otherArgs})
	if s.repeatedHashActive {
		t.Fatalf("flag should clear when pattern breaks")
	}

	// Recurrence re-fires.
	observeCall(t, e, st, now.Add(200*time.Millisecond), protocol.ToolCallPayload{ToolID: "tc-5", Name: "fake:do", Args: otherArgs})
	observeCall(t, e, st, now.Add(250*time.Millisecond), protocol.ToolCallPayload{ToolID: "tc-6", Name: "fake:do", Args: otherArgs})
	if !s.repeatedHashActive {
		t.Fatalf("recurrence should re-arm the rising edge")
	}

	// Sanity check: at least one stuck_nudge frame emitted.
	nudges := 0
	for _, f := range st.emittedFrames() {
		if sm, ok := f.(*protocol.SystemMessage); ok && sm.Payload.Kind == protocol.SystemMessageStuckNudge {
			nudges++
		}
	}
	if nudges == 0 {
		t.Fatalf("expected at least one stuck_nudge emit")
	}
}

func TestLocalToolHash_Stable(t *testing.T) {
	a := LocalToolHash("fake:do", map[string]any{"x": 1, "y": "z"})
	b := LocalToolHash("fake:do", map[string]any{"x": 1, "y": "z"})
	if a == "" || a != b {
		t.Errorf("hash unstable: a=%q b=%q", a, b)
	}
	c := LocalToolHash("fake:do", map[string]any{"x": 2})
	if c == a {
		t.Errorf("hash collision on different args: %q", c)
	}
	d := LocalToolHash("other:do", map[string]any{"x": 1, "y": "z"})
	if d == a {
		t.Errorf("hash collision on different tool name: %q", d)
	}
}

func TestStuckDetector_RepeatedErrorClusters(t *testing.T) {
	e := newTestExtension(t)
	st := newFakeState(t, "ses-r1")
	if err := e.InitState(context.Background(), st); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	s := FromState(st)
	now := time.Now()

	observe := func(tool string, n int, errCode string) {
		toolID := tool + "-tc"
		args := map[string]any{"n": n}
		observeCall(t, e, st, now, protocol.ToolCallPayload{ToolID: toolID, Name: tool, Args: args})
		observeResult(t, e, st, toolID, errCode)
		now = now.Add(50 * time.Millisecond)
	}

	observe("session:spawn_wave", 1, "bad_request")
	observe("session:wait_subagents", 1, "")
	if s.repeatedErrorActive {
		t.Fatalf("active too early — only one matching error")
	}

	observe("session:spawn_wave", 2, "bad_request")
	observe("session:wait_subagents", 2, "")
	if s.repeatedErrorActive {
		t.Fatalf("active too early — two matching errors")
	}

	observe("session:spawn_wave", 3, "bad_request")
	if !s.repeatedErrorActive {
		t.Fatalf("rising edge missed on third matching error")
	}

	observe("session:wait_subagents", 3, "")
	if s.repeatedErrorActive {
		t.Fatalf("flag should clear when latest sample succeeds")
	}

	observe("session:spawn_subagent", 1, "bad_request")
	observe("session:spawn_subagent", 2, "bad_request")
	if s.repeatedErrorActive {
		t.Fatalf("active across different tools — should cluster per (tool, code)")
	}
	observe("session:spawn_subagent", 3, "bad_request")
	if !s.repeatedErrorActive {
		t.Fatalf("rising edge missed on second cluster (different tool)")
	}
}

func TestStuckBuffer_FIFOTrim(t *testing.T) {
	e := newTestExtension(t)
	st := newFakeState(t, "ses-trim")
	if err := e.InitState(context.Background(), st); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	s := FromState(st)
	now := time.Now()
	for i := 0; i < 50; i++ {
		observeCall(t, e, st, now, protocol.ToolCallPayload{
			ToolID: "tc",
			Name:   "fake:do",
			Args:   map[string]any{"i": i},
		})
	}
	want := stuckRepeatedHashWindow
	if stuckTightDensityCount > want {
		want = stuckTightDensityCount
	}
	if stuckRepeatedErrorWindow > want {
		want = stuckRepeatedErrorWindow
	}
	s.mu.Lock()
	got := len(s.recentHashes)
	s.mu.Unlock()
	if got != want {
		t.Errorf("recentHashes len = %d, want trim to %d", got, want)
	}
}
