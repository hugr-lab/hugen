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
	role   string
	skill  string
	tier   string // empty defaults to "root"
	values sync.Map
	parent extension.SessionState
	// tools is the per-session ToolManager. nil for the executor
	// tests that never dispatch; hook tests set it to a real manager
	// with a registered fake provider.
	tools *tool.ToolManager
}

func newFakeState(id string) *fakeState {
	return &fakeState{id: id}
}

func (s *fakeState) SessionID() string                  { return s.id }
func (s *fakeState) SubagentName() string               { return "" }
func (s *fakeState) Role() string                       { return s.role }
func (s *fakeState) Skill() string                      { return s.skill }
func (s *fakeState) Depth() int                         { return 0 }
func (s *fakeState) Tier() string {
	if s.tier != "" {
		return s.tier
	}
	return "root"
}
func (s *fakeState) Parent() (extension.SessionState, bool) {
	if s.parent == nil {
		return nil, false
	}
	return s.parent, true
}
func (s *fakeState) Children() []extension.SessionState { return nil }
func (s *fakeState) Tools() *tool.ToolManager           { return s.tools }
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

// recordingTerminator captures the session ids RunWave asks to cancel.
type recordingTerminator struct {
	mu        sync.Mutex
	cancelled []string
}

func (t *recordingTerminator) terminate(_ context.Context, sessionID, _ string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cancelled = append(t.cancelled, sessionID)
	return nil
}

func (t *recordingTerminator) ids() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.cancelled...)
}

// TestExecutor_RunWave_PerWorkerTimeout verifies the per-role timeout
// semantics: a worker that overruns ITS budget is cancelled (via the
// terminator) and recorded `timeout`, while a sibling that lands in
// time succeeds — the wave is partial, not abandoned. (B20-followup /
// op2023 dogfood: the old single wave-level timeout failed the whole
// wave on the slowest worker and never cancelled it.)
func TestExecutor_RunWave_PerWorkerTimeout(t *testing.T) {
	state := newFakeState("mis-pw")
	m := installMissionState(state)

	var slowMu sync.Mutex
	var slowID string

	spawner := &fakeSpawner{}
	spawner.ingestion = func(req SpawnRequest, sessionID string) {
		switch req.Name {
		case "fast":
			ref, _ := MakeRef("fast", "w1")
			m.Handoffs.Put(Handoff{
				Ref: ref, Kind: KindHandoff, Status: "ok",
				Body: "done", CreatedAt: time.Now(),
			})
		case "slow":
			// Never lands a handoff — must time out + be cancelled.
			slowMu.Lock()
			slowID = sessionID
			slowMu.Unlock()
		}
	}

	term := &recordingTerminator{}
	exec := NewExecutor(spawner.spawn, nil).WithTerminator(term.terminate)

	wave := Wave{
		Label: "w1",
		Subagents: []SubagentSpec{
			{Name: "fast", Role: "fast", Task: "t"},
			{Name: "slow", Role: "slow", Task: "t"},
		},
	}
	roleTimeout := func(role string) time.Duration {
		if role == "slow" {
			return 30 * time.Millisecond
		}
		return 10 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, outcomes, err := exec.RunWave(ctx, state, wave,
		RunWaveOptions{RoleTimeout: roleTimeout})
	if err != nil {
		t.Fatalf("RunWave: unexpected err: %v", err)
	}
	if status != WaveStatusPartial {
		t.Errorf("status = %q, want partial", status)
	}

	byName := map[string]DoneWorker{}
	for _, o := range outcomes {
		byName[o.Name] = o
	}
	if byName["fast"].Status != "ok" {
		t.Errorf("fast status = %q, want ok", byName["fast"].Status)
	}
	if byName["slow"].Status != "timeout" || !byName["slow"].TimedOut {
		t.Errorf("slow outcome = %+v, want status=timeout TimedOut=true", byName["slow"])
	}

	slowMu.Lock()
	wantID := slowID
	slowMu.Unlock()
	if got := term.ids(); len(got) != 1 || got[0] != wantID {
		t.Errorf("terminator cancelled = %v, want [%s]", got, wantID)
	}
}

// TestExecutor_RunWave_HITLWaitingPausesDeadline covers the A6 fix: a worker
// parked on a HITL inquiry (e.g. the planner inside validate_and_approve
// waiting for the user to approve) must NOT be cancelled when its wall-clock
// budget passes — human-wait time is paused. Here the approver stays blocked
// well past its 30ms role budget, then unblocks and hands off; it must finish
// `ok`, never `timeout`, and the terminator must not fire.
func TestExecutor_RunWave_HITLWaitingPausesDeadline(t *testing.T) {
	state := newFakeState("mis-hitl")
	m := installMissionState(state)

	spawner := &fakeSpawner{}
	spawner.ingestion = func(req SpawnRequest, sessionID string) {
		switch req.Name {
		case "fast":
			ref, _ := MakeRef("fast", "w1")
			m.Handoffs.Put(Handoff{
				Ref: ref, Kind: KindHandoff, Status: "ok",
				Body: "done", CreatedAt: time.Now(),
			})
		case "approver":
			// Park on HITL well past the 30ms role budget, then hand off. Land
			// the handoff BEFORE clearing the HITL mark so the next poll sees a
			// completed worker (store.Get is checked before the deadline) —
			// deterministic, no clear↔timeout race. Without the pause this
			// worker would be cancelled at 30ms, long before the 120ms handoff.
			m.MarkHITLWaiting(sessionID)
			go func() {
				time.Sleep(120 * time.Millisecond)
				ref, _ := MakeRef("approver", "w1")
				m.Handoffs.Put(Handoff{
					Ref: ref, Kind: KindHandoff, Status: "ok",
					Body: "approved", CreatedAt: time.Now(),
				})
				m.ClearHITLWaiting(sessionID)
			}()
		}
	}

	term := &recordingTerminator{}
	exec := NewExecutor(spawner.spawn, nil).WithTerminator(term.terminate)

	wave := Wave{
		Label: "w1",
		Subagents: []SubagentSpec{
			{Name: "fast", Role: "fast", Task: "t"},
			{Name: "approver", Role: "approver", Task: "t"},
		},
	}
	roleTimeout := func(role string) time.Duration {
		if role == "approver" {
			return 30 * time.Millisecond
		}
		return 10 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, outcomes, err := exec.RunWave(ctx, state, wave,
		RunWaveOptions{RoleTimeout: roleTimeout})
	if err != nil {
		t.Fatalf("RunWave: unexpected err: %v", err)
	}
	if status != WaveStatusOk {
		t.Errorf("status = %q, want ok (HITL pause kept the approver alive past its budget)", status)
	}

	byName := map[string]DoneWorker{}
	for _, o := range outcomes {
		byName[o.Name] = o
	}
	if byName["approver"].Status != "ok" || byName["approver"].TimedOut {
		t.Errorf("approver outcome = %+v, want ok + NOT timed out (deadline paused while HITL-blocked)", byName["approver"])
	}
	if got := term.ids(); len(got) != 0 {
		t.Errorf("terminator cancelled %v, want none (a HITL-blocked worker must not be cancelled)", got)
	}
}

// TestExtension_InitState_WorkerInheritsMissionState verifies the
// shadowing fix: a worker-tier child does NOT get its own MissionState
// (which would hide the mission's), so FromState — and thus
// mission:get_research / get_handoff — resolves the MISSION's state.
// The mission-tier dispatcher still owns its own (covers nested
// missions).
func TestExtension_InitState_WorkerInheritsMissionState(t *testing.T) {
	ext := &Extension{}
	ctx := context.Background()

	mission := &fakeState{id: "mis", tier: "mission"}
	if err := ext.InitState(ctx, mission); err != nil {
		t.Fatalf("InitState(mission): %v", err)
	}
	mMission := FromState(mission)
	if mMission == nil {
		t.Fatal("mission-tier session must own a MissionState")
	}
	mMission.SetResearchOutput("findings here", nil, nil, nil)

	worker := &fakeState{id: "w", tier: "worker", parent: mission}
	if err := ext.InitState(ctx, worker); err != nil {
		t.Fatalf("InitState(worker): %v", err)
	}
	if _, ok := worker.Value(StateKey); ok {
		t.Error("worker must NOT install its own MissionState (would shadow the mission's)")
	}
	if FromState(worker) != mMission {
		t.Error("FromState(worker) must resolve to the mission's MissionState")
	}
	if findings, _, _ := FromState(worker).ResearchOutput(); findings != "findings here" {
		t.Errorf("worker sees research findings = %q, want 'findings here'", findings)
	}

	// A nested mission-tier child still owns its own (no sharing).
	nested := &fakeState{id: "nest", tier: "mission", parent: worker}
	if err := ext.InitState(ctx, nested); err != nil {
		t.Fatalf("InitState(nested): %v", err)
	}
	if FromState(nested) == mMission {
		t.Error("nested mission-tier session must own a SEPARATE MissionState")
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
