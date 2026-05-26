package session

import (
	"context"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	missionext "github.com/hugr-lab/hugen/pkg/extension/mission"
	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
	sessionstore "github.com/hugr-lab/hugen/pkg/session/store"
)

// TestMissionPDCA_RunWave_EndToEnd is the α.2 integration test:
// validates that the mission ext + Executor primitive work against
// a real *Session.
//
// Topology:
//
//	root (test parent, no turn driven)
//	└── mission (spawned via root.Spawn; mission ext attached; no
//	             turn driven — supervisor LLM is not exercised in
//	             Phase A)
//	    ├── worker-1 (scripted model emits a handoff block; final
//	    │             turn closes the session, ChildFrameObserver
//	    │             on mission ingests the handoff)
//	    └── worker-2 (same)
//
// Asserts:
//   - both workers' handoffs land in mission's Handoffs store.
//   - Executor.RunWave returns WaveStatusOk with two ok outcomes.
//   - PlanState.Done has one entry (the wave) and currentWave is
//     cleared.
//   - SpawnSpec.RenderMode plumbing fires: every spawned worker had
//     its asyncSpawnMode tagged with protocol.SubagentRenderSilent.
func TestMissionPDCA_RunWave_EndToEnd(t *testing.T) {
	// Scripted handoff text — both workers emit the same payload.
	// The handoff fence is mandatory (output_contract parser
	// rejects non-fenced text); the body keeps things minimal.
	handoffText := "```handoff\n" +
		`{"status":"ok","body":"echo ack","memory_summary":"echoed back"}` +
		"\n```"

	store := fixture.NewTestStore()
	ext := missionext.NewExtension(missionext.Config{AgentID: "a1"})
	root, cleanup := newTestParent(t,
		withTestStore(store),
		withTestRunLoop(),
		withTestExtensions(ext),
	)
	defer cleanup()

	// Swap the no-op default model for our handoff-emitting one.
	// The router is shared across the whole spawn tree (root +
	// every descendant), so workers spawned later pick up the new
	// model automatically. Root + mission sessions never run a
	// turn in this test, so the swap only affects workers.
	mdl := &scriptedModel{
		chunks: []model.Chunk{
			{Content: ptr(handoffText), Final: true},
		},
	}
	router := newRouterWithModel(t, mdl)
	root.models = router
	if root.deps != nil {
		root.deps.Models = router
	}

	// Open the mission session as a direct child of root. In
	// production this is what spawn_mission does; here we drive
	// it manually to keep the test focused on the executor path.
	// deps.Extensions is shared with root (children inherit it),
	// so mission ext's InitState already ran for the mission
	// session by the time Spawn returns.
	ctx := context.Background()
	mission, err := root.Spawn(ctx, SpawnSpec{
		Name: "mission-1",
		Task: "echo hello",
	})
	if err != nil {
		t.Fatalf("spawn mission: %v", err)
	}
	drainOutboxOnce(root.Outbox()) // subagent_started{mission}

	// Spawner closure — translates mission.SpawnRequest to
	// session.SpawnSpec + delivers the task as the worker's first
	// user message. Production wiring (α.3) puts this in
	// pkg/runtime; for α.2 we keep it inline.
	spawner := func(ctx context.Context, parent extension.SessionState, req missionext.SpawnRequest) (missionext.SpawnResult, error) {
		child, err := mission.Spawn(ctx, SpawnSpec{
			Name:       req.Name,
			Skill:      req.Skill,
			Role:       req.Role,
			Task:       req.Task,
			Inputs:     req.Inputs,
			RenderMode: req.RenderMode,
		})
		if err != nil {
			return missionext.SpawnResult{}, err
		}
		first := protocol.NewUserMessage(child.ID(), mission.agent.Participant(), req.Task)
		settled := child.Submit(ctx, first)
		return missionext.SpawnResult{
			SessionID: child.ID(),
			Settled:   settled,
		}, nil
	}

	wave := missionext.Wave{
		Label: "wave-1",
		Subagents: []missionext.SubagentSpec{
			{Name: "w1", Task: "say hi"},
			{Name: "w2", Task: "say bye"},
		},
	}

	exec := missionext.NewExecutor(spawner, nil)
	waveCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	status, outcomes, err := exec.RunWave(waveCtx, mission, wave, missionext.RunWaveOptions{})
	if err != nil {
		t.Fatalf("RunWave: %v", err)
	}
	if status != missionext.WaveStatusOk {
		t.Fatalf("wave status = %q, want ok (outcomes=%+v)", status, outcomes)
	}
	if len(outcomes) != 2 {
		t.Fatalf("outcomes len = %d, want 2", len(outcomes))
	}
	for _, o := range outcomes {
		if o.Status != "ok" {
			t.Errorf("worker %q status = %q, want ok (err=%q)", o.Name, o.Status, o.Error)
		}
	}

	// Handoffs store should have both refs.
	m := missionext.FromState(mission)
	if m == nil {
		t.Fatal("FromState(mission) = nil")
	}
	for _, ref := range []string{"w1@wave-1", "w2@wave-1"} {
		h, ok := m.Handoffs.Get(ref)
		if !ok {
			t.Errorf("Handoffs.Get(%q): not found", ref)
			continue
		}
		if h.Status != "ok" {
			t.Errorf("handoff %q status = %q, want ok", ref, h.Status)
		}
		if h.MemorySummary != "echoed back" {
			t.Errorf("handoff %q memory_summary = %q, want 'echoed back'", ref, h.MemorySummary)
		}
		if h.Kind != missionext.KindHandoff {
			t.Errorf("handoff %q kind = %q, want handoff", ref, h.Kind)
		}
		if h.Subagent.SessionID == "" {
			t.Errorf("handoff %q Subagent.SessionID empty", ref)
		}
	}

	// PlanState: one Done wave; no Active.
	if len(m.Plan.Done) != 1 || m.Plan.Done[0].Label != "wave-1" {
		t.Errorf("Plan.Done = %+v, want one wave-1 entry", m.Plan.Done)
	}
	if m.CurrentWave() != "" {
		t.Errorf("CurrentWave after RunWave = %q, want empty", m.CurrentWave())
	}

	// Confirm the two worker sessions were registered via
	// session_events. By the time RunWave returns, the workers
	// have produced their terminal frame; whether they're still
	// in mission.children or already deregistered depends on the
	// pump's handleSubagentResult path racing the wait loop, so
	// we read from the persisted event log instead.
	events, err := store.ListEvents(ctx, mission.ID(), sessionstore.ListEventsOpts{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	startCount := 0
	for _, ev := range events {
		if ev.EventType == "subagent_started" {
			startCount++
		}
	}
	if startCount != 2 {
		t.Errorf("mission subagent_started events = %d, want 2", startCount)
	}

	// SpawnSpec.RenderMode plumbing check — every worker's
	// terminal subagent_result event on the mission session
	// should carry RenderMode=silent, proving the spec field
	// reached child.asyncSpawnMode and was copied into the
	// projected SubagentResult payload by the pump.
	//
	// We may need a beat for the pump to publish — RunWave's
	// own wait loop returns as soon as Handoffs lands, which
	// happens BEFORE the pump's projectChildFrame builds the
	// SubagentResult. Poll with a short deadline.
	silentSeen := 0
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && silentSeen < 2 {
		evs, err := store.ListEvents(ctx, mission.ID(), sessionstore.ListEventsOpts{})
		if err != nil {
			t.Fatalf("ListEvents (poll): %v", err)
		}
		silentSeen = 0
		for _, ev := range evs {
			if ev.EventType != string(protocol.KindSubagentResult) {
				continue
			}
			if v, ok := ev.Metadata["render_mode"].(string); ok && v == protocol.SubagentRenderSilent {
				silentSeen++
			}
		}
		if silentSeen == 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if silentSeen != 2 {
		t.Errorf("subagent_result events with render_mode=silent = %d, want 2 — RenderMode plumbing may be broken", silentSeen)
	}
}

// TestMissionPDCA_RunMission_TerminatesMission is the α.4 integration
// test: validates that the full driveMission goroutine — wave loop,
// wave_complete emit, synthesis worker, terminal AgentMessage —
// produces a mission session that cleanly tears itself down via the
// parent's normal handleSubagentResult / SessionClose pipeline.
//
// Topology:
//
//	root (test parent, no turn driven)
//	└── mission (mission ext attached + RunMission'd; receives no
//	             UserMessage — the auto-runner drives the entire flow)
//	    ├── w1 (wave-1 worker; emits a handoff)
//	    └── synthesizer (_synthesis wave; emits a final handoff whose
//	                     body becomes the mission's AgentMessage text)
//
// Asserts:
//   - mission.Done() closes within a deadline (mission terminated).
//   - mission's event log contains at least two extension_frame
//     rows with op=wave_complete (one per wave, one per _synthesis).
//   - root sees a subagent_result with Reason="completed" and a
//     non-empty Result string (the synthesizer's body).
func TestMissionPDCA_RunMission_TerminatesMission(t *testing.T) {
	handoffText := "```handoff\n" +
		`{"status":"ok","body":"synthesis result","memory_summary":"summed up"}` +
		"\n```"

	store := fixture.NewTestStore()
	missionManifest := &missionext.MissionManifest{
		Name:    "echo-mission",
		Summary: "Phase A α.4 fixture",
		Plan: missionext.MissionPlanManifest{
			Inline: &missionext.InlinePlan{
				Waves: []missionext.Wave{
					{
						Label: "wave-1",
						Subagents: []missionext.SubagentSpec{
							{Name: "w1", Task: "say hi"},
						},
					},
				},
			},
		},
		Synthesis: missionext.SynthesisManifest{Role: "synthesizer"},
	}
	ext := missionext.NewExtension(missionext.Config{
		AgentID: "a1",
		Catalog: missionext.NewStaticCatalog(missionManifest),
	})
	root, cleanup := newTestParent(t,
		withTestStore(store),
		withTestRunLoop(),
		withTestExtensions(ext),
	)
	defer cleanup()

	// Every spawned worker (wave-1's w1 and _synthesis's synthesizer)
	// runs the same scripted handoff-emitting model. Root + mission
	// never run a turn — root is a passive parent and the mission's
	// supervisor LLM is not exercised in Phase A.
	mdl := &scriptedModel{
		chunks: []model.Chunk{
			{Content: ptr(handoffText), Final: true},
		},
	}
	router := newRouterWithModel(t, mdl)
	root.models = router
	if root.deps != nil {
		root.deps.Models = router
	}

	ctx := context.Background()
	mission, err := root.Spawn(ctx, SpawnSpec{
		Name:  "mission-1",
		Skill: "echo-mission",
		Task:  "synthesize hello",
	})
	if err != nil {
		t.Fatalf("spawn mission: %v", err)
	}

	if err := ext.RunMission(ctx, mission, "echo-mission", "synthesize hello", nil); err != nil {
		t.Fatalf("RunMission: %v", err)
	}

	select {
	case <-mission.Done():
	case <-time.After(10 * time.Second):
		t.Fatalf("mission did not terminate within 10s")
	}

	// Mission's event log should carry wave_complete ExtensionFrames
	// for both wave-1 and _synthesis.
	events, err := store.ListEvents(ctx, mission.ID(), sessionstore.ListEventsOpts{})
	if err != nil {
		t.Fatalf("ListEvents(mission): %v", err)
	}
	waveCompletes := make(map[string]bool)
	for _, ev := range events {
		if ev.EventType != string(protocol.KindExtensionFrame) {
			continue
		}
		op, _ := ev.Metadata["op"].(string)
		if op != "wave_complete" {
			continue
		}
		// payload data carries the wave label
		data, _ := ev.Metadata["data"].(map[string]any)
		if label, _ := data["label"].(string); label != "" {
			waveCompletes[label] = true
		}
	}
	if !waveCompletes["wave-1"] {
		t.Errorf("missing wave_complete for wave-1 (events=%v)", waveCompletes)
	}
	if !waveCompletes["_synthesis"] {
		t.Errorf("missing wave_complete for _synthesis (events=%v)", waveCompletes)
	}

	// Root must have seen a subagent_result for the mission carrying
	// the synthesizer's body as Result text. mission.Done() closes
	// when the mission's goroutine exits, but root's routeInbound
	// runs the persist call AFTER waiting on child.Done(), so the
	// event row may land a beat later. Poll briefly.
	sawMissionResult := false
	deadline := time.Now().Add(2 * time.Second)
	var resultText string
	for time.Now().Before(deadline) && !sawMissionResult {
		rootEvents, err := store.ListEvents(ctx, root.ID(), sessionstore.ListEventsOpts{})
		if err != nil {
			t.Fatalf("ListEvents(root): %v", err)
		}
		for _, ev := range rootEvents {
			if ev.EventType != string(protocol.KindSubagentResult) {
				continue
			}
			sid, _ := ev.Metadata["session_id"].(string)
			if sid != mission.ID() {
				continue
			}
			sawMissionResult = true
			resultText = ev.Content
			break
		}
		if sawMissionResult {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !sawMissionResult {
		t.Errorf("root never saw subagent_result for mission %s", mission.ID())
	} else if resultText != "synthesis result" {
		t.Errorf("mission subagent_result Result = %q, want %q (synthesizer body)",
			resultText, "synthesis result")
	}
}
