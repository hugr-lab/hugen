package mission

import (
	"context"
	"io/fs"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/assets"
	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/prompts"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// renderedFakeState is fakeState plus a Prompts() override —
// planner.go's buildPlannerTask reaches for the renderer; tests
// either supply the production one (assets.PromptsFS) or a
// fstest.MapFS for golden assertions.
type renderedFakeState struct {
	fakeState
	renderer *prompts.Renderer
}

func newRenderedFakeState(id string, renderer *prompts.Renderer) *renderedFakeState {
	rs := &renderedFakeState{renderer: renderer}
	rs.id = id
	return rs
}

func (s *renderedFakeState) Prompts() *prompts.Renderer { return s.renderer }

// productionRenderer returns a Renderer rooted at the embedded
// assets.PromptsFS/prompts subtree — same surface the runtime
// wires in production.
func productionRenderer(t *testing.T) *prompts.Renderer {
	t.Helper()
	sub, err := fs.Sub(assets.PromptsFS, "prompts")
	if err != nil {
		t.Fatalf("fs.Sub(assets.PromptsFS, prompts): %v", err)
	}
	return prompts.NewRenderer(sub, slog.Default())
}

// plannerFakeSpawner drives the planner loop end-to-end inside a
// unit test. Each spawn fires a per-role callback to inject the
// handoff bytes the executor expects, simulating the
// ChildFrameObserver path. The callbacks are role-aware so a
// single fixture can produce a planner kind=plan handoff for
// `_plan-*` waves and regular kind=handoff payloads for everything
// else.
type plannerFakeSpawner struct {
	mu          sync.Mutex
	nextID      atomic.Int64
	requests    []SpawnRequest
	plannerStep int // 1-indexed; bumped on each planner spawn.

	// onPlannerSpawn returns the handoff body the planner emits for
	// this iteration. Called with the 1-indexed iteration number.
	onPlannerSpawn func(iteration int) Handoff

	// onWorkerSpawn returns the handoff body a non-planner worker
	// emits. Called with the spawn request.
	onWorkerSpawn func(req SpawnRequest) Handoff

	// state references the parent mission state; the spawner stamps
	// handoffs into it directly (skipping the OnChildFrame path).
	state *renderedFakeState
}

func (f *plannerFakeSpawner) spawn(_ context.Context, _ extension.SessionState, req SpawnRequest) (SpawnResult, error) {
	id := newID(int(f.nextID.Add(1)))
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()

	m := FromState(f.state)
	if m == nil {
		panic("plannerFakeSpawner: state has no MissionState")
	}
	wave := m.CurrentWave()
	ref, _ := MakeRef(req.Name, wave)

	// Register the worker so the executor's collectOutcomes path
	// can attribute a session id.
	m.RegisterWorker(id, workerCursor{Name: req.Name, Role: req.Role, Skill: req.Skill})

	var h Handoff
	if strings.HasPrefix(wave, plannerWaveLabelPrefix) {
		f.mu.Lock()
		f.plannerStep++
		step := f.plannerStep
		f.mu.Unlock()
		if f.onPlannerSpawn != nil {
			h = f.onPlannerSpawn(step)
		}
	} else if f.onWorkerSpawn != nil {
		h = f.onWorkerSpawn(req)
	}
	h.Ref = ref
	h.Subagent = SubagentRef{SessionID: id, Name: req.Name, Role: req.Role, Skill: req.Skill}
	if h.CreatedAt.IsZero() {
		h.CreatedAt = time.Now()
	}

	// Drop the handoff into the store from a goroutine so the
	// executor's wait loop is exercised (matches the production
	// ChildFrameObserver path which fires on a separate goroutine).
	go func() {
		m.Handoffs.Put(h)
	}()

	settled := make(chan struct{})
	close(settled)
	return SpawnResult{SessionID: id, Settled: settled}, nil
}

// newPlannerExtension returns an Extension with a no-op logger so
// the planner loop's warn-level logs don't pollute test output.
func newPlannerExtension() *Extension {
	return NewExtension(Config{AgentID: "a1", Logger: slog.New(slog.NewTextHandler(noopWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))})
}

type noopWriter struct{}

func (noopWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestPlannerLoop_HappyPath_TwoIterations(t *testing.T) {
	state := newRenderedFakeState("mis-planner-1", productionRenderer(t))
	installMissionState(&state.fakeState)

	manifest := MissionManifest{
		Name: "planner-mission",
		Plan: MissionPlanManifest{
			Role:     "planner",
			Approval: NormalizePlanApproval(PlanApproval{Initial: ApprovalInitialSkip}),
			MaxWaves: 5,
		},
	}

	spawner := &plannerFakeSpawner{state: state}
	spawner.onPlannerSpawn = func(iteration int) Handoff {
		switch iteration {
		case 1:
			return Handoff{
				Kind:   KindPlan,
				Status: "ok",
				Body: map[string]any{
					"next_wave": map[string]any{
						"label": "wave-1",
						"subagents": []any{
							map[string]any{
								"name": "w1",
								"role": "echo",
								"task": "do thing",
							},
						},
					},
					"roadmap":   []any{},
					"rationale": "first wave",
				},
			}
		default:
			return Handoff{
				Kind:   KindPlan,
				Status: "ok",
				Body: map[string]any{
					"next_wave": nil,
					"roadmap":   []any{},
					"rationale": "done",
				},
			}
		}
	}
	spawner.onWorkerSpawn = func(_ SpawnRequest) Handoff {
		return Handoff{
			Kind:   KindHandoff,
			Status: "ok",
			Body:   "worker output",
		}
	}

	ext := newPlannerExtension()
	executor := NewExecutor(spawner.spawn, ext.logger)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	aborted, err := ext.runPlannerLoop(ctx, executor, state, manifest, manifest.Name, "achieve goal")
	if err != nil {
		t.Fatalf("runPlannerLoop: %v", err)
	}
	if aborted {
		t.Fatalf("aborted = true, want false")
	}

	// Two planner spawns + one wave-1 spawn = 3 requests, in order:
	// _plan-1, wave-1, _plan-2.
	if len(spawner.requests) != 3 {
		t.Fatalf("spawn requests = %d, want 3 (got %+v)", len(spawner.requests), waveNames(spawner.requests))
	}
}

func TestPlannerLoop_PlanComplete_OnFirstIteration(t *testing.T) {
	state := newRenderedFakeState("mis-planner-2", productionRenderer(t))
	installMissionState(&state.fakeState)

	manifest := MissionManifest{
		Name: "no-op-mission",
		Plan: MissionPlanManifest{
			Role:     "planner",
			Approval: NormalizePlanApproval(PlanApproval{Initial: ApprovalInitialSkip}),
			MaxWaves: 5,
		},
	}

	spawner := &plannerFakeSpawner{state: state}
	spawner.onPlannerSpawn = func(_ int) Handoff {
		return Handoff{
			Kind:   KindPlan,
			Status: "ok",
			Body: map[string]any{
				"next_wave": nil,
				"roadmap":   []any{},
				"rationale": "nothing to do",
			},
		}
	}

	ext := newPlannerExtension()
	executor := NewExecutor(spawner.spawn, ext.logger)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	aborted, err := ext.runPlannerLoop(ctx, executor, state, manifest, manifest.Name, "no-op")
	if err != nil {
		t.Fatalf("runPlannerLoop: %v", err)
	}
	if aborted {
		t.Fatal("aborted = true, want false")
	}
	if len(spawner.requests) != 1 {
		t.Errorf("spawn requests = %d, want 1 planner spawn only", len(spawner.requests))
	}
}

func TestPlannerLoop_MaxWavesCap(t *testing.T) {
	state := newRenderedFakeState("mis-planner-3", productionRenderer(t))
	installMissionState(&state.fakeState)

	manifest := MissionManifest{
		Name: "looper",
		Plan: MissionPlanManifest{
			Role:     "planner",
			Approval: NormalizePlanApproval(PlanApproval{Initial: ApprovalInitialSkip}),
			MaxWaves: 2,
		},
	}

	spawner := &plannerFakeSpawner{state: state}
	spawner.onPlannerSpawn = func(iteration int) Handoff {
		return Handoff{
			Kind:   KindPlan,
			Status: "ok",
			Body: map[string]any{
				"next_wave": map[string]any{
					"label": "wave-" + numToStr(iteration),
					"subagents": []any{
						map[string]any{"name": "w", "role": "echo", "task": "t"},
					},
				},
				"roadmap":   []any{},
				"rationale": "keep going",
			},
		}
	}
	spawner.onWorkerSpawn = func(_ SpawnRequest) Handoff {
		return Handoff{Kind: KindHandoff, Status: "ok", Body: "ok"}
	}

	ext := newPlannerExtension()
	executor := NewExecutor(spawner.spawn, ext.logger)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	aborted, err := ext.runPlannerLoop(ctx, executor, state, manifest, manifest.Name, "loop")
	if err != nil {
		t.Fatalf("runPlannerLoop: %v", err)
	}
	if aborted {
		t.Fatal("aborted = true, want false at cap")
	}
	// 2 iterations × (1 planner + 1 wave worker) = 4 spawns.
	if len(spawner.requests) != 4 {
		t.Errorf("spawn requests = %d, want 4 at cap (got %+v)", len(spawner.requests), waveNames(spawner.requests))
	}
}

func TestPlannerLoop_AbortsOnDecodeFailure(t *testing.T) {
	state := newRenderedFakeState("mis-planner-4", productionRenderer(t))
	installMissionState(&state.fakeState)

	manifest := MissionManifest{
		Name: "bad-planner",
		Plan: MissionPlanManifest{
			Role:     "planner",
			Approval: NormalizePlanApproval(PlanApproval{Initial: ApprovalInitialSkip}),
			MaxWaves: 3,
		},
	}

	spawner := &plannerFakeSpawner{state: state}
	spawner.onPlannerSpawn = func(_ int) Handoff {
		// Wrong kind — kind=handoff masquerading as a planner reply.
		// runPlannerLoop should reject it and abort.
		return Handoff{
			Kind:   KindHandoff,
			Status: "ok",
			Body:   "I am not a plan",
		}
	}

	ext := newPlannerExtension()
	executor := NewExecutor(spawner.spawn, ext.logger)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	aborted, err := ext.runPlannerLoop(ctx, executor, state, manifest, manifest.Name, "bad")
	if err == nil {
		t.Fatal("runPlannerLoop: want error from bad planner handoff, got nil")
	}
	if !aborted {
		t.Fatalf("aborted = false, want true")
	}
	var pe *PlannerError
	if !errAs(err, &pe) {
		t.Fatalf("err = %T (%v), want *PlannerError", err, err)
	}
	if pe.Iteration != 1 {
		t.Errorf("PlannerError.Iteration = %d, want 1", pe.Iteration)
	}
}

func TestApprovalRequiredForIteration(t *testing.T) {
	cases := []struct {
		name      string
		policy    PlanApproval
		iteration int
		want      bool
	}{
		{"defaults: initial=true", PlanApproval{}, 1, true},
		{"defaults: iter 2 = false", PlanApproval{}, 2, false},
		{"initial=skip: iter 1 = false", PlanApproval{Initial: ApprovalInitialSkip}, 1, false},
		{"iteration=always: iter 5 = true", PlanApproval{Iteration: ApprovalIterationAlways}, 5, true},
		{"iteration=never: iter 5 = false", PlanApproval{Iteration: ApprovalIterationNever}, 5, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := approvalRequiredForIteration(tc.policy, tc.iteration)
			if got != tc.want {
				t.Errorf("approvalRequiredForIteration(%+v, %d) = %v, want %v", tc.policy, tc.iteration, got, tc.want)
			}
		})
	}
}

func TestBuildPlannerTask_RendersApprovalDirective(t *testing.T) {
	state := newRenderedFakeState("mis-task", productionRenderer(t))
	manifest := MissionManifest{
		Name: "test",
		Plan: MissionPlanManifest{
			Role:     "planner",
			Approval: PlanApproval{Initial: ApprovalInitialRequired, Iteration: ApprovalIterationInitOnly},
			MaxWaves: 7,
		},
	}
	task, err := buildPlannerTask(state, manifest, "do the thing", 1)
	if err != nil {
		t.Fatalf("buildPlannerTask: %v", err)
	}
	for _, want := range []string{"do the thing", "[approval_required]", "session:inquire", "```plan", "1 of 7"} {
		if !strings.Contains(task, want) {
			t.Errorf("planner task missing %q. Task:\n%s", want, task)
		}
	}

	// Iteration 2 with initial-only — no approval directive.
	task2, err := buildPlannerTask(state, manifest, "go", 2)
	if err != nil {
		t.Fatalf("buildPlannerTask iter 2: %v", err)
	}
	if strings.Contains(task2, "[approval_required]") {
		t.Errorf("iter 2 task should NOT carry approval directive:\n%s", task2)
	}
}

// errAs is a thin wrapper around errors.As that takes any pointer
// — keeps the test bodies less cluttered.
func errAs(err error, target any) bool {
	type targetErr interface{ Unwrap() error }
	// Manual As-like walk to avoid pulling in errors twice in the
	// test file; PlannerError implements Unwrap.
	for cur := err; cur != nil; {
		if pe, ok := cur.(*PlannerError); ok {
			if t, ok := target.(**PlannerError); ok {
				*t = pe
				return true
			}
		}
		un, ok := cur.(targetErr)
		if !ok {
			return false
		}
		cur = un.Unwrap()
	}
	return false
}

// waveNames extracts the wave label of every recorded spawn. Used
// in error messages so a failed assertion shows exactly which
// spawns happened.
func waveNames(reqs []SpawnRequest) []string {
	out := make([]string, len(reqs))
	for i, r := range reqs {
		out[i] = r.Name
	}
	return out
}

// Phase B planner.go uses tool.ToolManager indirectly via
// SessionState.Tools(); the fakeState in executor_test.go returns
// nil for that, which is fine for the planner tests. Compile-time
// pin to keep the surface honest if a future planner.go grows to
// call Tools().
var _ tool.ToolProvider = (*Extension)(nil)

// protocol-import sentinel — planner.go reaches for ExtensionFrame
// constants; keep the import live so a future remove there doesn't
// silently strip our test's NewExtension dependency.
var _ = protocol.CategoryOp
