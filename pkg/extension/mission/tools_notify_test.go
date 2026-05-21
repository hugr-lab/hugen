package mission

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/prompts"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// rootWithChild is the test variant of fakeState used to exercise
// callNotify — it owns a stable Children() projection so the
// callNotify resolver can find the mission by Subagent name or
// session id.
type rootWithChild struct {
	id           string
	depth        int
	subagentName string
	values       sync.Map
	parent       extension.SessionState
	children     []extension.SessionState
}

func (s *rootWithChild) SessionID() string                  { return s.id }
func (s *rootWithChild) SubagentName() string               { return s.subagentName }
func (s *rootWithChild) Role() string                       { return "" }
func (s *rootWithChild) Skill() string                      { return "" }
func (s *rootWithChild) Depth() int                         { return s.depth }
func (s *rootWithChild) Parent() (extension.SessionState, bool) {
	if s.parent == nil {
		return nil, false
	}
	return s.parent, true
}
func (s *rootWithChild) Children() []extension.SessionState { return s.children }
func (s *rootWithChild) Tools() *tool.ToolManager           { return nil }
func (s *rootWithChild) Prompts() *prompts.Renderer         { return nil }
func (s *rootWithChild) Value(name string) (any, bool) {
	v, ok := s.values.Load(name)
	return v, ok
}
func (s *rootWithChild) SetValue(name string, value any)     { s.values.Store(name, value) }
func (s *rootWithChild) Emit(_ context.Context, _ protocol.Frame) error           { return nil }
func (s *rootWithChild) IsClosed() bool                                           { return false }
func (s *rootWithChild) Submit(_ context.Context, _ protocol.Frame) <-chan struct{} { return nil }
func (s *rootWithChild) OutboxOnly(_ context.Context, _ protocol.Frame) error     { return nil }
func (s *rootWithChild) ToolCatalogTokens(_ context.Context) int                  { return 0 }
func (s *rootWithChild) Extensions() []extension.Extension                        { return nil }
func (s *rootWithChild) RequestInquiry(_ context.Context, _ protocol.InquiryRequestPayload) (*protocol.InquiryResponse, error) {
	return nil, nil
}

func TestCallNotify_DeliversToPlanContext(t *testing.T) {
	ext := newPlannerExtension()

	missionState := &rootWithChild{id: "mis-1", subagentName: "echo-mission", depth: 1}
	missionState.SetValue(StateKey, NewMissionState())

	root := &rootWithChild{
		id:       "root-1",
		depth:    0,
		children: []extension.SessionState{missionState},
	}

	dispatchCtx := extension.WithSessionState(context.Background(), root)
	args, _ := json.Marshal(notifyInput{Name: "echo-mission", Text: "also check column foo"})
	resp, err := ext.callNotify(dispatchCtx, args)
	if err != nil {
		t.Fatalf("callNotify: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("unmarshal resp: %v", err)
	}
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("response.ok = %v, want true (resp=%s)", out["ok"], resp)
	}
	if sid, _ := out["session_id"].(string); sid != "mis-1" {
		t.Errorf("response.session_id = %q, want mis-1", sid)
	}

	m := FromState(missionState)
	if m == nil {
		t.Fatal("FromState(mission): nil")
	}
	rows := m.PlanContext.List()
	if len(rows) != 1 {
		t.Fatalf("plan_context len = %d, want 1", len(rows))
	}
	if rows[0].Phase != "user-followup" {
		t.Errorf("Phase = %q, want user-followup", rows[0].Phase)
	}
	if rows[0].Summary != "also check column foo" {
		t.Errorf("Summary = %q", rows[0].Summary)
	}
}

func TestCallNotify_RejectsNonRootCaller(t *testing.T) {
	ext := newPlannerExtension()
	notRoot := &rootWithChild{id: "mis-1", depth: 1}
	notRoot.SetValue(StateKey, NewMissionState())
	dispatchCtx := extension.WithSessionState(context.Background(), notRoot)
	args, _ := json.Marshal(notifyInput{Name: "x", Text: "y"})
	resp, err := ext.callNotify(dispatchCtx, args)
	if err != nil {
		t.Fatalf("callNotify: %v", err)
	}
	var env toolErrorResponse
	if err := json.Unmarshal(resp, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Error.Code != "forbidden" {
		t.Errorf("Error.Code = %q, want forbidden", env.Error.Code)
	}
}

func TestCallNotify_NotFoundOnUnknownName(t *testing.T) {
	ext := newPlannerExtension()
	root := &rootWithChild{id: "root-1", depth: 0, children: nil}
	dispatchCtx := extension.WithSessionState(context.Background(), root)
	args, _ := json.Marshal(notifyInput{Name: "ghost", Text: "hi"})
	resp, err := ext.callNotify(dispatchCtx, args)
	if err != nil {
		t.Fatalf("callNotify: %v", err)
	}
	var env toolErrorResponse
	if err := json.Unmarshal(resp, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Error.Code != "not_found" {
		t.Errorf("Error.Code = %q, want not_found", env.Error.Code)
	}
}

func TestCallNotify_BadRequestOnEmptyArgs(t *testing.T) {
	ext := newPlannerExtension()
	root := &rootWithChild{id: "root-1", depth: 0}
	dispatchCtx := extension.WithSessionState(context.Background(), root)
	args, _ := json.Marshal(notifyInput{Name: "", Text: "hi"})
	resp, err := ext.callNotify(dispatchCtx, args)
	if err != nil {
		t.Fatalf("callNotify: %v", err)
	}
	var env toolErrorResponse
	if err := json.Unmarshal(resp, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Error.Code != "bad_request" {
		t.Errorf("Error.Code = %q, want bad_request", env.Error.Code)
	}
}
