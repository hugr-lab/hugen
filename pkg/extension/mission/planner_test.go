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

	// Phase I — canned response for the runtime-driven approval
	// inquire. Tests that exercise the approval gate install one
	// before invoking runPlannerLoop. inquiryRequests records
	// every payload the loop sent so the test can assert on the
	// rendered question.
	inquiryResp     *protocol.InquiryResponse
	inquiryErr      error
	inquiryRequests []protocol.InquiryRequestPayload
}

func newRenderedFakeState(id string, renderer *prompts.Renderer) *renderedFakeState {
	rs := &renderedFakeState{renderer: renderer}
	rs.id = id
	return rs
}

func (s *renderedFakeState) Prompts() *prompts.Renderer { return s.renderer }

func (s *renderedFakeState) RequestInquiry(_ context.Context, payload protocol.InquiryRequestPayload) (*protocol.InquiryResponse, error) {
	s.inquiryRequests = append(s.inquiryRequests, payload)
	if s.inquiryErr != nil {
		return nil, s.inquiryErr
	}
	if s.inquiryResp != nil {
		clone := *s.inquiryResp
		clone.Payload.RequestID = payload.RequestID
		clone.Payload.CallerSessionID = s.id
		return &clone, nil
	}
	approved := true
	return &protocol.InquiryResponse{
		Payload: protocol.InquiryResponsePayload{
			RequestID:       payload.RequestID,
			CallerSessionID: s.id,
			Approved:        &approved,
		},
	}, nil
}

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
	checkerStep int // 1-indexed; bumped on each checker spawn.

	// onPlannerSpawn returns the handoff body the planner emits for
	// this iteration. Called with the 1-indexed iteration number.
	onPlannerSpawn func(iteration int) Handoff

	// onCheckerSpawn returns the handoff body the checker emits.
	// Called with the 1-indexed iteration number. Phase C.
	onCheckerSpawn func(iteration int) Handoff

	// onWorkerSpawn returns the handoff body a non-planner worker
	// emits. Called with the spawn request.
	onWorkerSpawn func(req SpawnRequest) Handoff

	// state references the parent mission state; the spawner stamps
	// handoffs into it directly (skipping the OnChildFrame path).
	state *renderedFakeState

	// autoMarkInquired, when true, calls MissionState.MarkInquired
	// on every planner spawn id so the approval gate accepts the
	// fake-emitted handoffs without a real InquiryRequest bubble.
	// Used by the approval-gate happy-path test.
	autoMarkInquired bool

	// autoMarkCheckerInquired, when true, flips the inquired flag
	// on every checker spawn id — the Phase-C equivalent for
	// verdict=inquire validation in unit tests.
	autoMarkCheckerInquired bool
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
	isPlanner := strings.HasPrefix(wave, plannerWaveLabelPrefix)
	isChecker := strings.HasPrefix(wave, checkerWaveLabelPrefix)
	switch {
	case isPlanner:
		f.mu.Lock()
		f.plannerStep++
		step := f.plannerStep
		f.mu.Unlock()
		if f.onPlannerSpawn != nil {
			h = f.onPlannerSpawn(step)
		}
		if f.autoMarkInquired {
			m.MarkInquired(id)
		}
	case isChecker:
		f.mu.Lock()
		f.checkerStep++
		step := f.checkerStep
		f.mu.Unlock()
		if f.onCheckerSpawn != nil {
			h = f.onCheckerSpawn(step)
		}
		if f.autoMarkCheckerInquired {
			m.MarkInquired(id)
		}
	default:
		if f.onWorkerSpawn != nil {
			h = f.onWorkerSpawn(req)
		}
	}
	h.Ref = ref
	h.Subagent = SubagentRef{SessionID: id, Name: req.Name, Role: req.Role, Skill: req.Skill}
	if h.CreatedAt.IsZero() {
		h.CreatedAt = time.Now()
	}

	// Drop the handoff into the store from a goroutine so the
	// executor's wait loop is exercised (matches the production
	// ChildFrameObserver path which fires on a separate goroutine).
	// Mirror the side-effects production's ingestHandoff applies
	// (B13: request reapproval on worker-flagged handoffs) so
	// tests exercise the same state transitions.
	go func() {
		m.Handoffs.Put(h)
		if invalidates, reason := invalidatesPlanApproval(h.Body); invalidates {
			m.RequestReapproval(reason)
		}
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
					"mission_goal":                "fixture",
					"mission_acceptance_criteria": []any{"fixture"},
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
				"mission_goal":                "fixture",
				"mission_acceptance_criteria": []any{"fixture"},
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

func TestPlannerLoop_RetriesOnDecodeFailureUntilCap(t *testing.T) {
	// Phase I — a planner that consistently emits an invalid
	// handoff (wrong kind, malformed JSON, missing required
	// field) no longer aborts the mission on the first failure.
	// Each parse failure folds into a synthetic verdict-amend so
	// the NEXT planner spawn sees the error under [Recent
	// verdict] and can re-emit.
	//
	// Phase 5.x — consecutive-error cap. After
	// [maxConsecutiveErrors] back-to-back failures the loop
	// aborts (aborted=true, nil error) so a stuck wave can't
	// monopolise the iteration budget. Synthesis then recaps
	// whatever partial state was produced.
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
		// runPlannerLoop should fold this into an amend verdict and
		// keep going until the iteration cap.
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
	if err != nil {
		t.Fatalf("runPlannerLoop: want nil error at consecutive-error cap, got %v", err)
	}
	if !aborted {
		t.Fatal("aborted = false, want true (consecutive-error cap should abort the loop)")
	}
	// After maxConsecutiveErrors back-to-back failures the loop
	// returns — so the planner spawns exactly that many times,
	// not the full max_waves budget.
	plannerSpawns := 0
	for _, r := range spawner.requests {
		if r.Name == "planner" {
			plannerSpawns++
		}
	}
	if plannerSpawns != maxConsecutiveErrors {
		t.Errorf("planner spawn count = %d, want %d (consecutive-error cap)", plannerSpawns, maxConsecutiveErrors)
	}
}

// Phase I — runtime-driven approval. Runtime issues the inquire
// from the mission session AFTER the planner emits its handoff;
// the planner no longer calls session:inquire itself. These
// tests cover (1) happy path: approved response lets the wave
// run; (2) deny path: rejection feeds a synthetic amend verdict
// into the next iteration; (3) the rendered question carries
// the typed plan body (next_wave + roadmap + rationale).

// Phase I.10 — approval is the planner's IN-TURN responsibility
// via mission:validate_plan(request_approval=true). Runtime
// enforces the gate post-close: handoff is rejected when approval
// was required but the planner skipped the tool, OR when the user
// denied and the planner shipped status=ok anyway. Both cases
// route to PlannerError → synthetic verdict-amend → replan.
//
// The tool handler in production calls MarkApprovalAttempt +
// (on approve) MarkIterationApproved. Tests simulate that by
// pre-marking the mission state inside the fake spawner before
// the handoff lands.

func TestPlannerLoop_ApprovalGate_HonouredOnApprove(t *testing.T) {
	state := newRenderedFakeState("mis-approval-ok", productionRenderer(t))
	installMissionState(&state.fakeState)

	manifest := MissionManifest{
		Name: "approval-required",
		Plan: MissionPlanManifest{
			Role:     "planner",
			Approval: PlanApproval{Initial: ApprovalInitialRequired, Iteration: ApprovalIterationInitOnly},
			MaxWaves: 2,
		},
	}

	iter1Body := map[string]any{
		"mission_goal":                "fixture",
		"mission_acceptance_criteria": []any{"fixture"},
		"next_wave": map[string]any{
			"label":     "wave-1",
			"subagents": []any{map[string]any{"name": "w", "role": "echo", "task": "t"}},
		},
		"roadmap":   []any{},
		"rationale": "approved",
	}

	spawner := &plannerFakeSpawner{state: state}
	spawner.onPlannerSpawn = func(iteration int) Handoff {
		// Simulate the in-turn tool path: validate_and_approve
		// called + user approved BEFORE the handoff is emitted
		// (flips the firstPlanApproved bit on mission state).
		if m := FromState(state); m != nil && iteration == 1 {
			m.MarkPlanApproved()
		}
		if iteration == 1 {
			return Handoff{
				Kind:   KindPlan,
				Status: "ok",
				Body:   iter1Body,
			}
		}
		return Handoff{
			Kind:   KindPlan,
			Status: "ok",
			Body:   map[string]any{"next_wave": nil, "roadmap": []any{}, "rationale": "done"},
		}
	}
	spawner.onWorkerSpawn = func(_ SpawnRequest) Handoff {
		return Handoff{Kind: KindHandoff, Status: "ok", Body: "ok"}
	}

	ext := newPlannerExtension()
	executor := NewExecutor(spawner.spawn, ext.logger)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	aborted, err := ext.runPlannerLoop(ctx, executor, state, manifest, manifest.Name, "x")
	if err != nil {
		t.Fatalf("runPlannerLoop: %v", err)
	}
	if aborted {
		t.Fatal("aborted = true, want false")
	}
}

func TestPlannerLoop_ApprovalGate_WorkerInvalidationReopensApproval(t *testing.T) {
	// Phase 5.x — B13. Runtime is skill-agnostic. The approval
	// gate trusts two bits on mission state: firstPlanApproved +
	// pendingReapproval. When a worker emits
	// `invalidates_plan_approval: true` in its handoff body, the
	// runtime flips pendingReapproval on; the next planner
	// iteration MUST re-call validate_and_approve and flip
	// pendingReapproval back off (via MarkPlanApproved) before
	// its handoff is accepted.
	state := newRenderedFakeState("mis-invalidate", productionRenderer(t))
	installMissionState(&state.fakeState)

	manifest := MissionManifest{
		Name: "invalidation",
		Plan: MissionPlanManifest{
			Role:     "planner",
			Approval: PlanApproval{Initial: ApprovalInitialRequired, Iteration: ApprovalIterationInitOnly},
			MaxWaves: 4,
		},
	}

	iter1Body := map[string]any{
		"mission_goal":                "fixture",
		"mission_acceptance_criteria": []any{"fixture"},
		"next_wave": map[string]any{
			"label":     "intake",
			"subagents": []any{map[string]any{"name": "scout", "role": "researcher", "task": "clarify scope"}},
		},
		"roadmap":   []any{},
		"rationale": "intake",
	}

	iter2Body := map[string]any{
		"mission_goal":                "fixture",
		"mission_acceptance_criteria": []any{"fixture"},
		"next_wave": map[string]any{
			"label":     "analyse",
			"subagents": []any{map[string]any{"name": "w", "role": "data-analyst", "task": "do"}},
		},
		"roadmap":   []any{},
		"rationale": "real plan",
	}

	spawner := &plannerFakeSpawner{state: state}
	spawner.onPlannerSpawn = func(iteration int) Handoff {
		switch iteration {
		case 1:
			if m := FromState(state); m != nil {
				m.MarkPlanApproved()
			}
			return Handoff{Kind: KindPlan, Status: "ok", Body: iter1Body}
		case 2:
			// After the researcher wave invalidated the prior
			// approval, pendingReapproval is true. Planner re-runs
			// validate_and_approve (simulated below) which clears
			// the pending flag.
			if m := FromState(state); m != nil {
				if pending, _ := m.PendingReapproval(); !pending {
					t.Errorf("iter-2 start: PendingReapproval = false, want true (researcher should have invalidated)")
				}
				m.MarkPlanApproved()
			}
			return Handoff{Kind: KindPlan, Status: "ok", Body: iter2Body}
		default:
			return Handoff{
				Kind:   KindPlan,
				Status: "ok",
				Body:   map[string]any{"next_wave": nil, "roadmap": []any{}, "rationale": "done"},
			}
		}
	}
	spawner.onWorkerSpawn = func(req SpawnRequest) Handoff {
		// Researcher wave's handoff invalidates the approval.
		if req.Role == "researcher" {
			return Handoff{
				Kind:   KindHandoff,
				Status: "ok",
				Body: map[string]any{
					"summary":                    "clarified scope",
					"invalidates_plan_approval":  true,
					"invalidates_reason":         "discovered new scope constraint",
				},
			}
		}
		return Handoff{Kind: KindHandoff, Status: "ok", Body: "ok"}
	}

	ext := newPlannerExtension()
	executor := NewExecutor(spawner.spawn, ext.logger)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	aborted, err := ext.runPlannerLoop(ctx, executor, state, manifest, manifest.Name, "x")
	if err != nil {
		t.Fatalf("runPlannerLoop: %v", err)
	}
	if aborted {
		t.Fatal("aborted = true, want false")
	}
}

func TestPlannerLoop_ApprovalGate_RejectsSkippedTool(t *testing.T) {
	// Phase 5.x — B13. Planner never calls
	// mission:validate_and_approve; runtime sees firstPlanApproved
	// = false on mission state → rejects with PlannerError →
	// synthetic verdict-amend → next iteration. The loop recovers
	// when a later planner spawn either calls the tool or signals
	// plan_complete.
	state := newRenderedFakeState("mis-approval-skip", productionRenderer(t))
	installMissionState(&state.fakeState)

	manifest := MissionManifest{
		Name: "approval-required",
		Plan: MissionPlanManifest{
			Role:     "planner",
			Approval: PlanApproval{Initial: ApprovalInitialRequired, Iteration: ApprovalIterationInitOnly},
			MaxWaves: 3,
		},
	}

	spawner := &plannerFakeSpawner{state: state}
	spawner.onPlannerSpawn = func(iteration int) Handoff {
		switch iteration {
		case 1:
			// No MarkApprovalAttempt → runtime rejects.
			return Handoff{
				Kind:   KindPlan,
				Status: "ok",
				Body: map[string]any{
					"mission_goal":                "fixture",
					"mission_acceptance_criteria": []any{"fixture"},
					"next_wave": map[string]any{
						"label":     "wave-1",
						"subagents": []any{map[string]any{"name": "w", "role": "echo", "task": "t"}},
					},
					"roadmap":   []any{},
					"rationale": "skipped approval",
				},
			}
		case 2:
			return Handoff{
				Kind:   KindPlan,
				Status: "ok",
				Body: map[string]any{
					"mission_goal":                "fixture",
					"mission_acceptance_criteria": []any{"fixture"},
					"next_wave": map[string]any{
						"label":     "wave-2",
						"subagents": []any{map[string]any{"name": "w", "role": "echo", "task": "t"}},
					},
					"roadmap":   []any{},
					"rationale": "post-amend",
				},
			}
		default:
			return Handoff{
				Kind:   KindPlan,
				Status: "ok",
				Body:   map[string]any{"next_wave": nil, "roadmap": []any{}, "rationale": "done"},
			}
		}
	}
	spawner.onWorkerSpawn = func(_ SpawnRequest) Handoff {
		return Handoff{Kind: KindHandoff, Status: "ok", Body: "ok"}
	}

	ext := newPlannerExtension()
	executor := NewExecutor(spawner.spawn, ext.logger)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	aborted, err := ext.runPlannerLoop(ctx, executor, state, manifest, manifest.Name, "x")
	if err != nil {
		t.Fatalf("runPlannerLoop: %v", err)
	}
	// Phase 5.x — every iteration in this test fails the approval
	// gate (no MarkApprovalAttempt). With the consecutive-error
	// cap at [maxConsecutiveErrors] the loop aborts cleanly after
	// that many back-to-back rejections — synthesis still runs on
	// whatever partial state exists. The recovery-on-iter-2 test
	// covers the happy path where a later iteration calls
	// validate_and_approve.
	if !aborted {
		t.Fatal("aborted = false, want true (consecutive-error cap should fire after every iter fails approval)")
	}
}

func TestPlannerLoop_ApprovalGate_RecoversAfterMissedApprovalOnIter2(t *testing.T) {
	// Phase 5.x — B13. Iter-1 planner ships without calling
	// validate_and_approve (firstPlanApproved stays false →
	// runtime rejects). Iter-2 simulates the proper in-turn tool
	// call by flipping the bit on mission state, then emits a
	// valid plan that flows through. Covers the recovery path.
	state := newRenderedFakeState("mis-approval-deny", productionRenderer(t))
	installMissionState(&state.fakeState)

	manifest := MissionManifest{
		Name: "approval-required",
		Plan: MissionPlanManifest{
			Role:     "planner",
			Approval: PlanApproval{Initial: ApprovalInitialRequired, Iteration: ApprovalIterationInitOnly},
			MaxWaves: 3,
		},
	}

	// Iter-1 planner ships without calling validate_and_approve —
	// firstPlanApproved is false on mission state, runtime rejects.
	// Iter-2 simulates the proper path: validate_and_approve runs
	// (MarkPlanApproved flips the bit) and the planner emits a
	// clean plan.
	spawner := &plannerFakeSpawner{state: state}
	spawner.onPlannerSpawn = func(iteration int) Handoff {
		switch iteration {
		case 1:
			// No MarkPlanApproved — runtime gate rejects.
			return Handoff{
				Kind:   KindPlan,
				Status: "ok",
				Body: map[string]any{
					"mission_goal":                "fixture",
					"mission_acceptance_criteria": []any{"fixture"},
					"next_wave": map[string]any{
						"label":     "wave-1",
						"subagents": []any{map[string]any{"name": "w", "role": "echo", "task": "t"}},
					},
					"roadmap":   []any{},
					"rationale": "shipped without calling validate_and_approve",
				},
			}
		case 2:
			// Iter-2 marks plan approved (simulating the in-turn
			// tool call) and emits a clean plan.
			if m := FromState(state); m != nil {
				m.MarkPlanApproved()
			}
			return Handoff{
				Kind:   KindPlan,
				Status: "ok",
				Body: map[string]any{
					"mission_goal":                "fixture",
					"mission_acceptance_criteria": []any{"fixture"},
					"next_wave": map[string]any{
						"label":     "wave-2",
						"subagents": []any{map[string]any{"name": "w", "role": "echo", "task": "t"}},
					},
					"roadmap":   []any{},
					"rationale": "revised",
				},
			}
		default:
			return Handoff{
				Kind:   KindPlan,
				Status: "ok",
				Body:   map[string]any{"next_wave": nil, "roadmap": []any{}, "rationale": "done"},
			}
		}
	}
	spawner.onWorkerSpawn = func(_ SpawnRequest) Handoff {
		return Handoff{Kind: KindHandoff, Status: "ok", Body: "ok"}
	}

	ext := newPlannerExtension()
	executor := NewExecutor(spawner.spawn, ext.logger)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	aborted, err := ext.runPlannerLoop(ctx, executor, state, manifest, manifest.Name, "x")
	if err != nil {
		t.Fatalf("runPlannerLoop: %v", err)
	}
	if aborted {
		t.Fatal("aborted = true, want false (replan should recover)")
	}
}

func TestPlannerLoop_Checker_Continue_FollowedByPlanComplete(t *testing.T) {
	state := newRenderedFakeState("mis-checker-cont", productionRenderer(t))
	installMissionState(&state.fakeState)

	manifest := MissionManifest{
		Name: "checker-continue",
		Plan: MissionPlanManifest{
			Role:     "planner",
			Approval: NormalizePlanApproval(PlanApproval{Initial: ApprovalInitialSkip}),
			MaxWaves: 5,
		},
		Control: ControlManifest{Role: "checker"},
	}

	spawner := &plannerFakeSpawner{state: state}
	spawner.onPlannerSpawn = func(iteration int) Handoff {
		switch iteration {
		case 1:
			return Handoff{
				Kind:   KindPlan,
				Status: "ok",
				Body: map[string]any{
					"mission_goal":                "fixture",
					"mission_acceptance_criteria": []any{"fixture"},
					"next_wave": map[string]any{
						"label":     "wave-1",
						"subagents": []any{map[string]any{"name": "w", "role": "echo", "task": "t"}},
					},
					"roadmap":   []any{},
					"rationale": "first wave",
				},
			}
		default:
			return Handoff{
				Kind:   KindPlan,
				Status: "ok",
				Body:   map[string]any{"next_wave": nil, "roadmap": []any{}, "rationale": "done"},
			}
		}
	}
	spawner.onWorkerSpawn = func(_ SpawnRequest) Handoff {
		return Handoff{Kind: KindHandoff, Status: "ok", Body: "ok"}
	}
	spawner.onCheckerSpawn = func(_ int) Handoff {
		return Handoff{
			Kind:   KindVerdict,
			Status: "ok",
			Body:   map[string]any{"decision": "continue", "reason": "on track"},
		}
	}

	ext := newPlannerExtension()
	executor := NewExecutor(spawner.spawn, ext.logger)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	aborted, err := ext.runPlannerLoop(ctx, executor, state, manifest, manifest.Name, "go")
	if err != nil {
		t.Fatalf("runPlannerLoop: %v", err)
	}
	if aborted {
		t.Fatal("aborted = true, want false")
	}
	// Expected spawn sequence: _plan-1, wave-1 (w), _check-1, _plan-2.
	if len(spawner.requests) != 4 {
		t.Fatalf("spawn requests = %d, want 4 (got %v)", len(spawner.requests), waveNames(spawner.requests))
	}
}

func TestPlannerLoop_SkipCheck_BypassesChecker(t *testing.T) {
	// Phase I.9 O3 — planner sets next_wave.skip_check: true on a
	// trivial wave; runtime must skip the checker spawn and go
	// straight to the next planner iteration. Spawn list should
	// then be: _plan-1, wave-1 (worker), _plan-2 — NO _check-1.
	state := newRenderedFakeState("mis-skipcheck", productionRenderer(t))
	installMissionState(&state.fakeState)

	manifest := MissionManifest{
		Name: "skip-check",
		Plan: MissionPlanManifest{
			Role:     "planner",
			Approval: NormalizePlanApproval(PlanApproval{Initial: ApprovalInitialSkip}),
			MaxWaves: 5,
		},
		Control: ControlManifest{Role: "checker"},
	}

	spawner := &plannerFakeSpawner{state: state}
	spawner.onPlannerSpawn = func(iteration int) Handoff {
		switch iteration {
		case 1:
			return Handoff{
				Kind:   KindPlan,
				Status: "ok",
				Body: map[string]any{
					"mission_goal":                "fixture",
					"mission_acceptance_criteria": []any{"fixture"},
					"next_wave": map[string]any{
						"label":      "wave-1",
						"subagents":  []any{map[string]any{"name": "w", "role": "echo", "task": "t"}},
						"skip_check": true,
					},
					"roadmap":   []any{},
					"rationale": "trivial — skip checker",
				},
			}
		default:
			return Handoff{
				Kind:   KindPlan,
				Status: "ok",
				Body:   map[string]any{"next_wave": nil, "roadmap": []any{}, "rationale": "done"},
			}
		}
	}
	spawner.onWorkerSpawn = func(_ SpawnRequest) Handoff {
		return Handoff{Kind: KindHandoff, Status: "ok", Body: "ok"}
	}
	spawner.onCheckerSpawn = func(_ int) Handoff {
		t.Fatal("checker should not be spawned when skip_check=true")
		return Handoff{}
	}

	ext := newPlannerExtension()
	executor := NewExecutor(spawner.spawn, ext.logger)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	aborted, err := ext.runPlannerLoop(ctx, executor, state, manifest, manifest.Name, "go")
	if err != nil {
		t.Fatalf("runPlannerLoop: %v", err)
	}
	if aborted {
		t.Fatal("aborted = true, want false")
	}
	// Expected: _plan-1, wave-1 (w), _plan-2 = 3 spawns. NO _check-1.
	if got := len(spawner.requests); got != 3 {
		t.Fatalf("spawn requests = %d, want 3 (got %v)", got, waveNames(spawner.requests))
	}
}

func TestPlannerLoop_Checker_Finish_ExitsLoopEarly(t *testing.T) {
	state := newRenderedFakeState("mis-checker-finish", productionRenderer(t))
	installMissionState(&state.fakeState)

	manifest := MissionManifest{
		Name: "checker-finish",
		Plan: MissionPlanManifest{
			Role:     "planner",
			Approval: NormalizePlanApproval(PlanApproval{Initial: ApprovalInitialSkip}),
			MaxWaves: 5,
		},
		Control: ControlManifest{Role: "checker"},
	}

	spawner := &plannerFakeSpawner{state: state}
	spawner.onPlannerSpawn = func(_ int) Handoff {
		return Handoff{
			Kind:   KindPlan,
			Status: "ok",
			Body: map[string]any{
				"mission_goal":                "fixture",
				"mission_acceptance_criteria": []any{"fixture"},
				"next_wave": map[string]any{
					"label":     "wave-1",
					"subagents": []any{map[string]any{"name": "w", "role": "echo", "task": "t"}},
				},
				"roadmap":   []any{},
				"rationale": "wave",
			},
		}
	}
	spawner.onWorkerSpawn = func(_ SpawnRequest) Handoff {
		return Handoff{Kind: KindHandoff, Status: "ok", Body: "ok"}
	}
	spawner.onCheckerSpawn = func(_ int) Handoff {
		// Phase I.26 — runtime gates `finish` on mission_ac_status;
		// the checker must show every criterion satisfied to exit
		// the loop early.
		return Handoff{
			Kind:   KindVerdict,
			Status: "ok",
			Body: map[string]any{
				"decision": "finish",
				"reason":   "satisfied",
				"mission_ac_status": []any{
					map[string]any{
						"criterion": "fixture",
						"satisfied": true,
						"evidence":  "wave-1 produced expected output",
					},
				},
			},
		}
	}

	ext := newPlannerExtension()
	executor := NewExecutor(spawner.spawn, ext.logger)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	aborted, err := ext.runPlannerLoop(ctx, executor, state, manifest, manifest.Name, "go")
	if err != nil {
		t.Fatalf("runPlannerLoop: %v", err)
	}
	if aborted {
		t.Fatal("aborted = true, want false")
	}
	// Single iteration only — planner + wave + checker. Loop exits
	// without spawning another planner.
	if len(spawner.requests) != 3 {
		t.Fatalf("spawn requests = %d, want 3 (got %v)", len(spawner.requests), waveNames(spawner.requests))
	}
}

func TestPlannerLoop_Checker_Amend_CarriesIssuesToNextPlanner(t *testing.T) {
	state := newRenderedFakeState("mis-checker-amend", productionRenderer(t))
	installMissionState(&state.fakeState)

	manifest := MissionManifest{
		Name: "checker-amend",
		Plan: MissionPlanManifest{
			Role:     "planner",
			Approval: NormalizePlanApproval(PlanApproval{Initial: ApprovalInitialSkip}),
			MaxWaves: 3,
		},
		Control: ControlManifest{Role: "checker"},
	}

	// Capture each planner spawn's rendered task — the SECOND
	// planner spawn must include a [Recent verdict] section with
	// the amend issues.
	plannerTasks := make([]string, 0)
	spawner := &plannerFakeSpawner{state: state}
	spawner.onPlannerSpawn = func(iteration int) Handoff {
		// Grab the most recent request's task body — the spawner
		// records it just before calling onPlannerSpawn.
		spawner.mu.Lock()
		if n := len(spawner.requests); n > 0 {
			plannerTasks = append(plannerTasks, spawner.requests[n-1].Task)
		}
		spawner.mu.Unlock()
		switch iteration {
		case 1:
			return Handoff{
				Kind:   KindPlan,
				Status: "ok",
				Body: map[string]any{
					"mission_goal":                "fixture",
					"mission_acceptance_criteria": []any{"fixture"},
					"next_wave": map[string]any{
						"label":     "wave-1",
						"subagents": []any{map[string]any{"name": "w", "role": "echo", "task": "t"}},
					},
					"roadmap":   []any{},
					"rationale": "first",
				},
			}
		default:
			return Handoff{
				Kind:   KindPlan,
				Status: "ok",
				Body:   map[string]any{"next_wave": nil, "roadmap": []any{}, "rationale": "done"},
			}
		}
	}
	spawner.onWorkerSpawn = func(_ SpawnRequest) Handoff {
		return Handoff{Kind: KindHandoff, Status: "ok", Body: "ok"}
	}
	spawner.onCheckerSpawn = func(_ int) Handoff {
		return Handoff{
			Kind:   KindVerdict,
			Status: "ok",
			Body: map[string]any{
				"decision": "amend",
				"issues":   []any{"wrong filter", "missing column"},
				"reason":   "replan",
			},
		}
	}

	ext := newPlannerExtension()
	executor := NewExecutor(spawner.spawn, ext.logger)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	aborted, err := ext.runPlannerLoop(ctx, executor, state, manifest, manifest.Name, "x")
	if err != nil {
		t.Fatalf("runPlannerLoop: %v", err)
	}
	if aborted {
		t.Fatal("aborted = true, want false")
	}
	if len(plannerTasks) < 2 {
		t.Fatalf("plannerTasks captured = %d, want 2+", len(plannerTasks))
	}
	// First planner task: no recent verdict block.
	if strings.Contains(plannerTasks[0], "[Recent verdict]") {
		t.Errorf("first planner task should not carry [Recent verdict]:\n%s", plannerTasks[0])
	}
	// Second planner task: carries amend issues verbatim.
	for _, want := range []string{"[Recent verdict]", "amend", "wrong filter", "missing column"} {
		if !strings.Contains(plannerTasks[1], want) {
			t.Errorf("second planner task missing %q:\n%s", want, plannerTasks[1])
		}
	}
}

func TestPlannerLoop_Checker_InquireRejectedWithoutInquiry(t *testing.T) {
	state := newRenderedFakeState("mis-checker-inq-reject", productionRenderer(t))
	installMissionState(&state.fakeState)

	manifest := MissionManifest{
		Name: "checker-inq",
		Plan: MissionPlanManifest{
			Role:     "planner",
			Approval: NormalizePlanApproval(PlanApproval{Initial: ApprovalInitialSkip}),
			MaxWaves: 3,
		},
		Control: ControlManifest{Role: "checker"},
	}

	spawner := &plannerFakeSpawner{state: state}
	spawner.onPlannerSpawn = func(_ int) Handoff {
		return Handoff{
			Kind:   KindPlan,
			Status: "ok",
			Body: map[string]any{
				"mission_goal":                "fixture",
				"mission_acceptance_criteria": []any{"fixture"},
				"next_wave": map[string]any{
					"label":     "wave-1",
					"subagents": []any{map[string]any{"name": "w", "role": "echo", "task": "t"}},
				},
				"roadmap":   []any{},
				"rationale": "first",
			},
		}
	}
	spawner.onWorkerSpawn = func(_ SpawnRequest) Handoff {
		return Handoff{Kind: KindHandoff, Status: "ok", Body: "ok"}
	}
	// Checker says inquire but autoMarkCheckerInquired is OFF — the
	// gate must reject.
	spawner.onCheckerSpawn = func(_ int) Handoff {
		return Handoff{
			Kind:   KindVerdict,
			Status: "ok",
			Body:   map[string]any{"decision": "inquire", "reason": "need user input"},
		}
	}

	ext := newPlannerExtension()
	executor := NewExecutor(spawner.spawn, ext.logger)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	aborted, err := ext.runPlannerLoop(ctx, executor, state, manifest, manifest.Name, "x")
	if err == nil {
		t.Fatal("runPlannerLoop: want error from missing-inquiry checker gate, got nil")
	}
	if !aborted {
		t.Fatal("aborted = false, want true")
	}
	if !strings.Contains(err.Error(), "session:inquire") {
		t.Errorf("err = %v, want substring 'session:inquire'", err)
	}
}

func TestBuildPlannerTask_RendersPlanContextSection(t *testing.T) {
	state := newRenderedFakeState("mis-pc", productionRenderer(t))
	m := installMissionState(&state.fakeState)
	m.PlanContext.Append(PlanContextEntry{
		Iteration: 1, Phase: "do", Summary: "explored orders table",
	})
	m.PlanContext.Append(PlanContextEntry{
		Iteration: 1, Phase: "verdict", Summary: "checker said amend",
	})
	manifest := MissionManifest{
		Name: "pc-test",
		Plan: MissionPlanManifest{
			Role:     "planner",
			Approval: PlanApproval{Initial: ApprovalInitialSkip, Iteration: ApprovalIterationNever},
			MaxWaves: 5,
		},
	}
	task, err := buildPlannerTask(state, manifest, "goal", 2, nil)
	if err != nil {
		t.Fatalf("buildPlannerTask: %v", err)
	}
	for _, want := range []string{"[Plan context]", "explored orders table", "checker said amend"} {
		if !strings.Contains(task, want) {
			t.Errorf("planner task missing %q. Task:\n%s", want, task)
		}
	}
}

// TestUnsatisfiedMissionAC covers the Phase I.26 runtime gate:
// the helper takes a checker verdict and returns the unsatisfied
// mission criteria as human-readable strings. Empty status array
// is treated as "the checker misbehaved" and surfaces a single
// reminder so the planner can ask for proper mission_ac_status
// next iteration.
func TestUnsatisfiedMissionAC(t *testing.T) {
	cases := []struct {
		name        string
		verdict     Verdict
		wantCount   int
		wantSubstr  []string
		wantEvidence []string
	}{
		{
			name:       "empty status produces guidance",
			verdict:    Verdict{},
			wantCount:  1,
			wantSubstr: []string{"mission_ac_status"},
		},
		{
			name: "all satisfied returns nil",
			verdict: Verdict{
				MissionACStatus: []ACCriterionStatus{
					{Criterion: "HTML saved", Satisfied: true, Evidence: "wrk@x path=/a"},
					{Criterion: "charts render", Satisfied: true, Evidence: "wrk@x carries svg"},
				},
			},
			wantCount: 0,
		},
		{
			name: "mixed surfaces only unsatisfied",
			verdict: Verdict{
				MissionACStatus: []ACCriterionStatus{
					{Criterion: "HTML saved", Satisfied: true, Evidence: "wrk@x path=/a"},
					{Criterion: "charts render", Satisfied: false, Evidence: "no svg in handoff body"},
					{Criterion: "data complete", Satisfied: false},
				},
			},
			wantCount:    2,
			wantSubstr:   []string{"charts render", "data complete"},
			wantEvidence: []string{"no svg in handoff body", "no evidence in handoffs"},
		},
		{
			name: "blank criterion gets placeholder name",
			verdict: Verdict{
				MissionACStatus: []ACCriterionStatus{
					{Criterion: "", Satisfied: false, Evidence: "blank"},
				},
			},
			wantCount:  1,
			wantSubstr: []string{"unnamed criterion"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := unsatisfiedMissionAC(tc.verdict)
			if len(got) != tc.wantCount {
				t.Fatalf("len(out) = %d, want %d (out=%v)", len(got), tc.wantCount, got)
			}
			for _, want := range tc.wantSubstr {
				found := false
				for _, line := range got {
					if strings.Contains(line, want) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("missing substring %q in %v", want, got)
				}
			}
			for _, ev := range tc.wantEvidence {
				found := false
				for _, line := range got {
					if strings.Contains(line, ev) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("missing evidence text %q in %v", ev, got)
				}
			}
		})
	}
}

// TestPlannerLoop_Checker_FinishWithUnsatisfiedAC_CoercesToAmend
// covers the Phase I.26 runtime gate inside runPlannerLoop. When
// the checker proposes `finish` while mission_ac_status carries
// any unsatisfied entry, the loop must coerce that verdict into a
// synthetic amend so the next planner spawn sees the gap. The
// recovery flows when the planner replans, the next checker
// closes the gap, and the loop exits cleanly.
func TestPlannerLoop_Checker_FinishWithUnsatisfiedAC_CoercesToAmend(t *testing.T) {
	state := newRenderedFakeState("mis-checker-finish-gated", productionRenderer(t))
	installMissionState(&state.fakeState)

	manifest := MissionManifest{
		Name: "checker-finish-gated",
		Plan: MissionPlanManifest{
			Role:     "planner",
			Approval: NormalizePlanApproval(PlanApproval{Initial: ApprovalInitialSkip}),
			MaxWaves: 5,
		},
		Control: ControlManifest{Role: "checker"},
	}

	plannerTasks := make([]string, 0)
	spawner := &plannerFakeSpawner{state: state}
	spawner.onPlannerSpawn = func(iteration int) Handoff {
		spawner.mu.Lock()
		if n := len(spawner.requests); n > 0 {
			plannerTasks = append(plannerTasks, spawner.requests[n-1].Task)
		}
		spawner.mu.Unlock()
		switch iteration {
		case 1, 2:
			return Handoff{
				Kind:   KindPlan,
				Status: "ok",
				Body: map[string]any{
					"mission_goal":                "deliver dashboard",
					"mission_acceptance_criteria": []any{"HTML saved", "charts render"},
					"next_wave": map[string]any{
						"label":     "wave-" + numToStr(iteration),
						"subagents": []any{map[string]any{"name": "w", "role": "echo", "task": "t"}},
					},
					"roadmap":   []any{},
					"rationale": "iter " + numToStr(iteration),
				},
			}
		default:
			return Handoff{
				Kind:   KindPlan,
				Status: "ok",
				Body:   map[string]any{"next_wave": nil, "roadmap": []any{}, "rationale": "done"},
			}
		}
	}
	spawner.onWorkerSpawn = func(_ SpawnRequest) Handoff {
		return Handoff{Kind: KindHandoff, Status: "ok", Body: "ok"}
	}
	// First checker: finish with unsatisfied AC → must be coerced.
	// Second checker: finish with everything satisfied → loop exits.
	spawner.onCheckerSpawn = func(iteration int) Handoff {
		switch iteration {
		case 1:
			return Handoff{
				Kind:   KindVerdict,
				Status: "ok",
				Body: map[string]any{
					"decision": "finish",
					"reason":   "looks done",
					"mission_ac_status": []any{
						map[string]any{"criterion": "HTML saved", "satisfied": true, "evidence": "w@wave-1 carries path"},
						map[string]any{"criterion": "charts render", "satisfied": false, "evidence": "no svg yet"},
					},
				},
			}
		default:
			return Handoff{
				Kind:   KindVerdict,
				Status: "ok",
				Body: map[string]any{
					"decision": "finish",
					"reason":   "now satisfied",
					"mission_ac_status": []any{
						map[string]any{"criterion": "HTML saved", "satisfied": true, "evidence": "w@wave-2 carries path"},
						map[string]any{"criterion": "charts render", "satisfied": true, "evidence": "w@wave-2 produced svg"},
					},
				},
			}
		}
	}

	ext := newPlannerExtension()
	executor := NewExecutor(spawner.spawn, ext.logger)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	aborted, err := ext.runPlannerLoop(ctx, executor, state, manifest, manifest.Name, "build dashboard")
	if err != nil {
		t.Fatalf("runPlannerLoop: %v", err)
	}
	if aborted {
		t.Fatal("aborted = true, want false")
	}
	// Expected spawn sequence: _plan-1, wave-1, _check-1 (finish→coerced to amend),
	// _plan-2, wave-2, _check-2 (finish, clean) = 6 spawns.
	if got := len(spawner.requests); got != 6 {
		t.Fatalf("spawn requests = %d, want 6 (got %v)", got, waveNames(spawner.requests))
	}
	// The SECOND planner task should carry the unsatisfied AC under
	// [Recent verdict] (coerced into a synthetic amend with the gap
	// in issues).
	if len(plannerTasks) < 2 {
		t.Fatalf("plannerTasks captured = %d, want 2+", len(plannerTasks))
	}
	for _, want := range []string{"[Recent verdict]", "amend", "charts render"} {
		if !strings.Contains(plannerTasks[1], want) {
			t.Errorf("second planner task missing %q:\n%s", want, plannerTasks[1])
		}
	}
}

func TestApprovalRequiredForIteration(t *testing.T) {
	// Phase I.23 — approval is policy-only and uniform across
	// iterations. The iteration arg is kept for telemetry but
	// unused. Only Initial=skip flips approval off (mission-level
	// opt-out for automation / tests).
	cases := []struct {
		name      string
		policy    PlanApproval
		iteration int
		want      bool
	}{
		{"defaults: iter 1 = true", PlanApproval{}, 1, true},
		{"defaults: iter 2 = true", PlanApproval{}, 2, true},
		{"initial=skip: iter 1 = false", PlanApproval{Initial: ApprovalInitialSkip}, 1, false},
		{"initial=skip: iter 5 = false", PlanApproval{Initial: ApprovalInitialSkip}, 5, false},
		{"iteration=always: iter 5 = true", PlanApproval{Iteration: ApprovalIterationAlways}, 5, true},
		{"iteration=never: iter 5 = true (kept; Initial governs)", PlanApproval{Iteration: ApprovalIterationNever}, 5, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := approvalRequiredForIteration(tc.policy, tc.iteration, nil)
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
	task, err := buildPlannerTask(state, manifest, "do the thing", 1, nil)
	if err != nil {
		t.Fatalf("buildPlannerTask: %v", err)
	}
	for _, want := range []string{"do the thing", "[approval_required]", "mission:validate_and_approve", "```plan", "1 of 7"} {
		if !strings.Contains(task, want) {
			t.Errorf("planner task missing %q. Task:\n%s", want, task)
		}
	}

	// Phase I.23 — approval is uniform across iterations under
	// policy.Initial!=skip; iter 2 also carries the directive.
	task2, err := buildPlannerTask(state, manifest, "go", 2, nil)
	if err != nil {
		t.Fatalf("buildPlannerTask iter 2: %v", err)
	}
	if !strings.Contains(task2, "[approval_required]") {
		t.Errorf("iter 2 task should ALSO carry approval directive under policy-only gate:\n%s", task2)
	}

	// initial=skip → no approval directive at any iteration.
	skipManifest := manifest
	skipManifest.Plan.Approval = PlanApproval{Initial: ApprovalInitialSkip}
	taskSkip, err := buildPlannerTask(state, skipManifest, "go", 1, nil)
	if err != nil {
		t.Fatalf("buildPlannerTask skip: %v", err)
	}
	if strings.Contains(taskSkip, "[approval_required]") {
		t.Errorf("initial=skip task should NOT carry approval directive:\n%s", taskSkip)
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
