package scheduler

import (
	"context"
	"sync"
	"testing"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/prompts"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// fakeState is the minimal extension.SessionState the policy tests
// need: a value bag, a parent pointer, and an emit recorder.
type fakeState struct {
	id      string
	values  sync.Map
	parent  extension.SessionState
	emitted []protocol.Frame
	emitMu  sync.Mutex
}

func newFakeState(id string) *fakeState { return &fakeState{id: id} }

func (s *fakeState) SessionID() string    { return s.id }
func (s *fakeState) SubagentName() string { return "" }
func (s *fakeState) Role() string         { return "" }
func (s *fakeState) Skill() string        { return "" }
func (s *fakeState) Depth() int           { return 0 }
func (s *fakeState) Parent() (extension.SessionState, bool) {
	if s.parent == nil {
		return nil, false
	}
	return s.parent, true
}
func (s *fakeState) Children() []extension.SessionState { return nil }
func (s *fakeState) Tools() *tool.ToolManager           { return nil }
func (s *fakeState) Prompts() *prompts.Renderer         { return nil }
func (s *fakeState) Value(name string) (any, bool) {
	v, ok := s.values.Load(name)
	return v, ok
}
func (s *fakeState) SetValue(name string, value any) { s.values.Store(name, value) }
func (s *fakeState) Emit(_ context.Context, f protocol.Frame) error {
	s.emitMu.Lock()
	s.emitted = append(s.emitted, f)
	s.emitMu.Unlock()
	return nil
}
func (s *fakeState) IsClosed() bool                                                  { return false }
func (s *fakeState) Submit(_ context.Context, _ protocol.Frame) <-chan struct{}      { return nil }
func (s *fakeState) OutboxOnly(_ context.Context, _ protocol.Frame) error            { return nil }
func (s *fakeState) ToolCatalogTokens(_ context.Context) int                         { return 0 }
func (s *fakeState) SessionUsage() *protocol.TokenUsage                              { return nil }
func (s *fakeState) Extensions() []extension.Extension                               { return nil }
func (s *fakeState) RequestInquiry(_ context.Context, _ protocol.InquiryRequestPayload) (*protocol.InquiryResponse, error) {
	return nil, nil
}

func (s *fakeState) emittedFrames() []protocol.Frame {
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	out := make([]protocol.Frame, len(s.emitted))
	copy(out, s.emitted)
	return out
}

// newExt builds a TaskManager extension wired to in-memory plumbing
// the policy tests need. Store + skills can be nil because the
// approval policy never reaches them.
func newExt(t *testing.T) *Extension {
	t.Helper()
	return NewExtension(nil, nil, "agt-test", nil)
}

func TestMaybeAutoApprove_OnCronSession_AllowedTool(t *testing.T) {
	ext := newExt(t)

	cron := newFakeState("ses-cron-1")
	cron.SetValue(protocol.SchedulerFireStateKey, &protocol.FireContext{
		TaskID:       "tsk_alpha",
		FireSeq:      3,
		AllowedTools: []string{"bash:exec", "notepad:append"},
	})

	taskID, ok := ext.MaybeAutoApprove(context.Background(), cron, "bash:exec")
	if !ok {
		t.Fatal("expected auto-approval")
	}
	if taskID != "tsk_alpha" {
		t.Errorf("granted task id = %q, want tsk_alpha", taskID)
	}

	frames := cron.emittedFrames()
	if len(frames) != 1 {
		t.Fatalf("expected 1 audit frame, got %d", len(frames))
	}
	ef, ok := frames[0].(*protocol.ExtensionFrame)
	if !ok {
		t.Fatalf("frame[0] type=%T, want *ExtensionFrame", frames[0])
	}
	if ef.Payload.Extension != "schedule" {
		t.Errorf("frame extension = %q, want schedule", ef.Payload.Extension)
	}
	if ef.Payload.Op != "tool_auto_granted_by_task" {
		t.Errorf("frame op = %q", ef.Payload.Op)
	}
}

func TestMaybeAutoApprove_OnCronSession_DeniedTool(t *testing.T) {
	ext := newExt(t)
	cron := newFakeState("ses-cron-2")
	cron.SetValue(protocol.SchedulerFireStateKey, &protocol.FireContext{
		TaskID:       "tsk_beta",
		AllowedTools: []string{"notepad:append"},
	})

	taskID, ok := ext.MaybeAutoApprove(context.Background(), cron, "bash:exec")
	if ok {
		t.Fatalf("expected NO auto-approval; got taskID=%q", taskID)
	}
	if got := len(cron.emittedFrames()); got != 0 {
		t.Errorf("audit frame emitted on deny; got %d frames", got)
	}
}

func TestMaybeAutoApprove_OnCronSession_EmptyAllowList(t *testing.T) {
	ext := newExt(t)
	cron := newFakeState("ses-cron-empty")
	cron.SetValue(protocol.SchedulerFireStateKey, &protocol.FireContext{
		TaskID:       "tsk_gamma",
		AllowedTools: []string{}, // explicit empty = no auto-approve
	})

	_, ok := ext.MaybeAutoApprove(context.Background(), cron, "bash:exec")
	if ok {
		t.Fatal("empty allow-list must NOT auto-approve")
	}
}

func TestMaybeAutoApprove_OnNonCronSession(t *testing.T) {
	ext := newExt(t)
	root := newFakeState("ses-root")
	// No SchedulerFireStateKey stamped.
	_, ok := ext.MaybeAutoApprove(context.Background(), root, "bash:exec")
	if ok {
		t.Fatal("root session must NOT auto-approve cron tools")
	}
}

func TestMaybeAutoApprove_ChildOfCron(t *testing.T) {
	ext := newExt(t)

	cron := newFakeState("ses-cron-parent")
	cron.SetValue(protocol.SchedulerFireStateKey, &protocol.FireContext{
		TaskID:       "tsk_chain",
		AllowedTools: []string{"hugr:query"},
	})
	worker := newFakeState("ses-worker")
	worker.parent = cron

	taskID, ok := ext.MaybeAutoApprove(context.Background(), worker, "hugr:query")
	if !ok {
		t.Fatal("worker under cron with allowed tool should auto-approve")
	}
	if taskID != "tsk_chain" {
		t.Errorf("granted task id = %q, want tsk_chain", taskID)
	}

	// Audit frame anchors on the cron session, not the worker.
	if got := len(worker.emittedFrames()); got != 0 {
		t.Errorf("audit frame should anchor on cron, not worker; worker got %d", got)
	}
	if got := len(cron.emittedFrames()); got != 1 {
		t.Errorf("audit frame should anchor on cron; got %d", got)
	}
}

func TestMaybeAutoApprove_NilCaller(t *testing.T) {
	ext := newExt(t)
	if _, ok := ext.MaybeAutoApprove(context.Background(), nil, "x:y"); ok {
		t.Fatal("nil caller must return (\"\", false)")
	}
}

func TestMaybeAutoApprove_ValueIsWrongType(t *testing.T) {
	ext := newExt(t)
	s := newFakeState("ses-bad-type")
	// Stash the wrong type under the key — defensive: must NOT crash
	// and must NOT auto-approve.
	s.SetValue(protocol.SchedulerFireStateKey, "not a fire context")

	if _, ok := ext.MaybeAutoApprove(context.Background(), s, "x:y"); ok {
		t.Fatal("malformed state value must not produce a grant")
	}
}
