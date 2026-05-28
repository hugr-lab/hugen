package mission

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/prompts"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// fakeState is the minimal extension.SessionState an executor test
// needs: a value bag + a parent pointer. Everything else returns
// zero values.
type fakeState struct {
	id     string
	values sync.Map
	parent extension.SessionState
}

func newFakeState(id string) *fakeState {
	return &fakeState{id: id}
}

func (s *fakeState) SessionID() string                  { return s.id }
func (s *fakeState) SubagentName() string               { return "" }
func (s *fakeState) Role() string                       { return "" }
func (s *fakeState) Skill() string                      { return "" }
func (s *fakeState) Depth() int                         { return 0 }
func (s *fakeState) Tier() string                       { return "root" }
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
func (s *fakeState) SetValue(name string, value any) {
	s.values.Store(name, value)
}
func (s *fakeState) Emit(_ context.Context, _ protocol.Frame) error           { return nil }
func (s *fakeState) IsClosed() bool                                           { return false }
func (s *fakeState) Submit(_ context.Context, _ protocol.Frame) <-chan struct{} { return nil }
func (s *fakeState) OutboxOnly(_ context.Context, _ protocol.Frame) error     { return nil }
func (s *fakeState) ToolCatalogTokens(_ context.Context) int                  { return 0 }
func (s *fakeState) SessionUsage() *protocol.TokenUsage                       { return nil }
func (s *fakeState) Extensions() []extension.Extension                        { return nil }
func (s *fakeState) RequestInquiry(_ context.Context, _ protocol.InquiryRequestPayload) (*protocol.InquiryResponse, error) {
	return nil, nil
}

// fakeSpawner is the executor's spawner double. It records every
// spawn request and triggers the user-supplied ingestion callback
// from a goroutine so the executor's wait loop sees handoffs land.
type fakeSpawner struct {
	mu         sync.Mutex
	requests   []SpawnRequest
	nextID     atomic.Int64
	ingestion  func(req SpawnRequest, sessionID string)
	spawnError error // when non-nil the spawner returns this error
}

func (f *fakeSpawner) spawn(ctx context.Context, parent extension.SessionState, req SpawnRequest) (SpawnResult, error) {
	if f.spawnError != nil {
		return SpawnResult{}, f.spawnError
	}
	id := newID(int(f.nextID.Add(1)))
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if f.ingestion != nil {
		go f.ingestion(req, id)
	}
	settled := make(chan struct{})
	close(settled)
	return SpawnResult{SessionID: id, Settled: settled}, nil
}

func newID(seq int) string {
	return "ses-" + time.Now().Format("150405") + "-" + numToStr(seq)
}

func numToStr(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [16]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// installMissionState attaches a fresh MissionState handle to
// state, matching what Extension.InitState would do.
func installMissionState(state extension.SessionState) *MissionState {
	m := NewMissionState()
	state.SetValue(StateKey, m)
	return m
}

// contextWithState wraps state into the standard dispatch ctx the
// tool handlers expect (mirrors extension.WithSessionState's
// production path).
func contextWithState(state extension.SessionState) context.Context {
	return extension.WithSessionState(context.Background(), state)
}

func TestExecutor_RunWave_HappyPath(t *testing.T) {
	state := newFakeState("mis-1")
	m := installMissionState(state)

	wave := Wave{
		Label: "wave-1",
		Subagents: []SubagentSpec{
			{Name: "w1", Role: "echo", Task: "say hi"},
			{Name: "w2", Role: "echo", Task: "say bye"},
		},
	}

	spawner := &fakeSpawner{}
	spawner.ingestion = func(req SpawnRequest, sessionID string) {
		// Simulate the ChildFrameObserver path: register the
		// worker, then land a handoff.
		m.RegisterWorker(sessionID, workerCursor{
			Name:  req.Name,
			Role:  req.Role,
			Skill: req.Skill,
		})
		// Pretend the worker emitted a handoff.
		ref, _ := MakeRef(req.Name, m.CurrentWave())
		m.Handoffs.Put(Handoff{
			Ref:    ref,
			Kind:   KindHandoff,
			Status: "ok",
			Body:   "ack: " + req.Task,
			Subagent: SubagentRef{
				SessionID: sessionID,
				Name:      req.Name,
				Role:      req.Role,
			},
			CreatedAt: time.Now(),
		})
	}

	exec := NewExecutor(spawner.spawn, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	status, outcomes, err := exec.RunWave(ctx, state, wave, RunWaveOptions{})
	if err != nil {
		t.Fatalf("RunWave: %v", err)
	}
	if status != WaveStatusOk {
		t.Fatalf("status = %q, want ok", status)
	}
	if len(outcomes) != 2 {
		t.Fatalf("outcomes len = %d, want 2", len(outcomes))
	}
	for _, w := range outcomes {
		if w.Status != "ok" {
			t.Errorf("worker %q status = %q, want ok", w.Name, w.Status)
		}
		if w.Ref == "" {
			t.Errorf("worker %q missing ref", w.Name)
		}
	}

	// Verify RenderMode default = silent on every spawn request.
	for _, r := range spawner.requests {
		if r.RenderMode != "silent" {
			t.Errorf("spawn req %q RenderMode = %q, want silent", r.Name, r.RenderMode)
		}
	}

	// Verify PlanState was updated.
	m.mu.Lock()
	done := m.Plan.Done
	active := m.currentWave
	m.mu.Unlock()
	if len(done) != 1 {
		t.Fatalf("Plan.Done len = %d, want 1", len(done))
	}
	if done[0].Label != "wave-1" || done[0].Status != WaveStatusOk {
		t.Errorf("DoneWave = %+v, want label=wave-1 status=ok", done[0])
	}
	if active != "" {
		t.Errorf("currentWave = %q, want empty after wave done", active)
	}
}

func TestExecutor_RunWave_SpawnError(t *testing.T) {
	state := newFakeState("mis-2")
	installMissionState(state)
	wave := Wave{
		Label: "w1",
		Subagents: []SubagentSpec{
			{Name: "w1", Task: "t"},
		},
	}
	spawner := &fakeSpawner{spawnError: errors.New("kaboom")}
	exec := NewExecutor(spawner.spawn, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	status, outcomes, err := exec.RunWave(ctx, state, wave, RunWaveOptions{})
	if err != nil {
		t.Fatalf("RunWave: unexpected error %v (executor should swallow spawn errs into outcomes)", err)
	}
	if status != WaveStatusFailed {
		t.Errorf("status = %q, want failed", status)
	}
	if len(outcomes) != 1 || outcomes[0].Status != "error" {
		t.Fatalf("outcomes = %+v, want one error entry", outcomes)
	}
	if !strings.Contains(outcomes[0].Error, "kaboom") {
		t.Errorf("outcome error = %q, want substring kaboom", outcomes[0].Error)
	}
}

func TestExecutor_RunWave_BadDependsOn(t *testing.T) {
	state := newFakeState("mis-3")
	installMissionState(state)
	wave := Wave{
		Label: "w1",
		Subagents: []SubagentSpec{
			{Name: "w1", Task: "t", DependsOn: []string{"missing@elsewhere"}},
		},
	}
	exec := NewExecutor((&fakeSpawner{}).spawn, nil)
	ctx := context.Background()
	status, _, err := exec.RunWave(ctx, state, wave, RunWaveOptions{})
	if err == nil {
		t.Fatal("RunWave: want error for missing dep, got nil")
	}
	if status != WaveStatusFailed {
		t.Errorf("status = %q, want failed", status)
	}
}

func TestExecutor_RunWave_Timeout(t *testing.T) {
	state := newFakeState("mis-4")
	installMissionState(state)
	wave := Wave{
		Label: "w1",
		Subagents: []SubagentSpec{
			{Name: "w1", Task: "t"},
		},
	}
	// Spawner that never lands a handoff — wait loop should time
	// out and the executor should return ctx.Err().
	spawner := &fakeSpawner{}
	exec := NewExecutor(spawner.spawn, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	status, _, err := exec.RunWave(ctx, state, wave, RunWaveOptions{})
	if err == nil {
		t.Fatal("RunWave: want ctx-timeout error, got nil")
	}
	if status != WaveStatusFailed {
		t.Errorf("status = %q, want failed", status)
	}
}

func TestExecutor_RunWave_DependsOnResolved(t *testing.T) {
	state := newFakeState("mis-5")
	m := installMissionState(state)

	// Seed a handoff from a prior wave.
	m.Handoffs.Put(Handoff{
		Ref:    "prior@w0",
		Kind:   KindHandoff,
		Status: "ok",
		Body:   "prior data here",
		Subagent: SubagentRef{
			Name: "prior",
			Role: "scout",
		},
		CreatedAt: time.Now(),
	})

	wave := Wave{
		Label: "w1",
		Subagents: []SubagentSpec{
			{Name: "consumer", Task: "use prior", DependsOn: []string{"prior@w0"}},
		},
	}

	spawner := &fakeSpawner{}
	spawner.ingestion = func(req SpawnRequest, sessionID string) {
		// Verify task got the [Resolved depends_on] prefix.
		if !strings.Contains(req.Task, "[Resolved depends_on]") {
			t.Errorf("spawn req.Task missing depends_on prefix:\n%s", req.Task)
		}
		if !strings.Contains(req.Task, "prior data here") {
			t.Errorf("spawn req.Task missing prior body:\n%s", req.Task)
		}
		m.RegisterWorker(sessionID, workerCursor{Name: req.Name})
		ref, _ := MakeRef(req.Name, m.CurrentWave())
		m.Handoffs.Put(Handoff{
			Ref:       ref,
			Kind:      KindHandoff,
			Status:    "ok",
			Body:      "consumed",
			CreatedAt: time.Now(),
		})
	}

	exec := NewExecutor(spawner.spawn, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, err := exec.RunWave(ctx, state, wave, RunWaveOptions{}); err != nil {
		t.Fatalf("RunWave: %v", err)
	}
}
