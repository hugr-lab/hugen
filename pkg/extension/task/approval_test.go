package task

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/prompts"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/tool"
)

func ptrBool(b bool) *bool { return &b }

func TestInterpretLaunchResponse(t *testing.T) {
	cases := []struct {
		name           string
		payload        protocol.InquiryResponsePayload
		wantApproved   bool
		wantAutoApprov bool
		wantRefine     string
	}{
		{"approve with tools", protocol.InquiryResponsePayload{Approved: ptrBool(true), AutoApproveTools: true}, true, true, ""},
		{"approve only", protocol.InquiryResponsePayload{Approved: ptrBool(true)}, true, false, ""},
		{"reject", protocol.InquiryResponsePayload{Approved: ptrBool(false)}, false, false, ""},
		{"refine (nil approved)", protocol.InquiryResponsePayload{Response: "use 2025 not 2024"}, false, false, "use 2025 not 2024"},
		// auto-approve flag is ignored unless approved (defensive)
		{"reject ignores auto flag", protocol.InquiryResponsePayload{Approved: ptrBool(false), AutoApproveTools: true}, false, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := interpretLaunchResponse(&protocol.InquiryResponse{Payload: tc.payload})
			if d.approved != tc.wantApproved {
				t.Errorf("approved = %v, want %v", d.approved, tc.wantApproved)
			}
			if d.autoApproveTools != tc.wantAutoApprov {
				t.Errorf("autoApproveTools = %v, want %v", d.autoApproveTools, tc.wantAutoApprov)
			}
			if d.refine != tc.wantRefine {
				t.Errorf("refine = %q, want %q", d.refine, tc.wantRefine)
			}
		})
	}
	if d := interpretLaunchResponse(nil); d.approved {
		t.Errorf("nil response must not be approved")
	}
}

func TestLaunchApprovalContext(t *testing.T) {
	withGoal := launchApprovalContext("Report road length by geozone")
	if !strings.Contains(withGoal, "Report road length") {
		t.Errorf("context dropped the goal: %q", withGoal)
	}
	if !strings.Contains(withGoal, "Approve with tools") {
		t.Errorf("context missing the mode explanation: %q", withGoal)
	}
	noGoal := launchApprovalContext("")
	if !strings.Contains(noGoal, "Approve with tools") {
		t.Errorf("empty-goal context missing the mode explanation: %q", noGoal)
	}
}

func TestMaybeAutoApprove_StampGrants(t *testing.T) {
	e := NewExtension(nil, "agt_test", nil)

	// Worker carrying the stamp → granted on the first hop.
	worker := newTaskFakeState("ses-worker")
	worker.SetValue(taskAutoApproveToolsKey, true)
	if id, ok := e.MaybeAutoApprove(context.Background(), worker, "bash:exec"); !ok || id != "ses-worker" {
		t.Errorf("stamped worker: got (%q,%v), want (ses-worker,true)", id, ok)
	}

	// Nested child of a stamped worker → granted via the parent walk.
	child := newTaskFakeState("ses-child")
	child.parent = worker
	if id, ok := e.MaybeAutoApprove(context.Background(), child, "hugr:query"); !ok || id != "ses-worker" {
		t.Errorf("child of stamped worker: got (%q,%v), want (ses-worker,true)", id, ok)
	}

	// No stamp anywhere → not granted (falls through to the modal).
	plain := newTaskFakeState("ses-plain")
	if _, ok := e.MaybeAutoApprove(context.Background(), plain, "bash:exec"); ok {
		t.Errorf("unstamped session must not be auto-approved")
	}

	// Stamp set to false (defensive: a non-true value never grants).
	off := newTaskFakeState("ses-off")
	off.SetValue(taskAutoApproveToolsKey, false)
	if _, ok := e.MaybeAutoApprove(context.Background(), off, "bash:exec"); ok {
		t.Errorf("false stamp must not be auto-approved")
	}

	// Nil caller → not granted, no panic.
	if _, ok := e.MaybeAutoApprove(context.Background(), nil, "x:y"); ok {
		t.Errorf("nil caller must not be auto-approved")
	}
}

// taskFakeState is the minimal extension.SessionState the approval-walk
// test needs: a value bag + a parent pointer. Mirrors the scheduler
// package's fakeState.
type taskFakeState struct {
	id     string
	values sync.Map
	parent extension.SessionState
}

func newTaskFakeState(id string) *taskFakeState { return &taskFakeState{id: id} }

func (s *taskFakeState) SessionID() string    { return s.id }
func (s *taskFakeState) SubagentName() string { return "" }
func (s *taskFakeState) Role() string         { return "" }
func (s *taskFakeState) Skill() string        { return "" }
func (s *taskFakeState) Depth() int           { return 0 }
func (s *taskFakeState) Tier() string         { return "worker" }
func (s *taskFakeState) Parent() (extension.SessionState, bool) {
	if s.parent == nil {
		return nil, false
	}
	return s.parent, true
}
func (s *taskFakeState) Children() []extension.SessionState { return nil }
func (s *taskFakeState) Tools() *tool.ToolManager           { return nil }
func (s *taskFakeState) Prompts() *prompts.Renderer         { return nil }
func (s *taskFakeState) Value(name string) (any, bool) {
	v, ok := s.values.Load(name)
	return v, ok
}
func (s *taskFakeState) SetValue(name string, value any)             { s.values.Store(name, value) }
func (s *taskFakeState) Emit(_ context.Context, _ protocol.Frame) error { return nil }
func (s *taskFakeState) IsClosed() bool                                 { return false }
func (s *taskFakeState) Submit(_ context.Context, _ protocol.Frame) <-chan struct{} {
	return nil
}
func (s *taskFakeState) OutboxOnly(_ context.Context, _ protocol.Frame) error { return nil }
func (s *taskFakeState) ToolCatalogTokens(_ context.Context) int             { return 0 }
func (s *taskFakeState) SessionUsage() *protocol.TokenUsage                  { return nil }
func (s *taskFakeState) Extensions() []extension.Extension                   { return nil }
func (s *taskFakeState) RequestInquiry(_ context.Context, _ protocol.InquiryRequestPayload) (*protocol.InquiryResponse, error) {
	return nil, nil
}
