package mission

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/tool"
)

// seqHookProvider returns a configurable sequence of result
// envelopes (the last repeats once exhausted) so a test can drive
// the check-gate's fail→retry→pass path. Records the call count.
type seqHookProvider struct {
	toolName string
	results  []json.RawMessage
	calls    atomic.Int32
}

func (p *seqHookProvider) Name() string            { return providerPrefix(p.toolName) }
func (p *seqHookProvider) Lifetime() tool.Lifetime { return tool.LifetimePerSession }
func (p *seqHookProvider) List(context.Context) ([]tool.Tool, error) {
	return []tool.Tool{{
		Name:             p.toolName,
		Provider:         providerPrefix(p.toolName),
		PermissionObject: "hugen:tool:" + providerPrefix(p.toolName),
	}}, nil
}
func (p *seqHookProvider) Call(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	i := int(p.calls.Add(1)) - 1
	if i >= len(p.results) {
		i = len(p.results) - 1
	}
	return p.results[i], nil
}
func (p *seqHookProvider) Subscribe(context.Context) (<-chan tool.ProviderEvent, error) {
	return nil, nil
}
func (p *seqHookProvider) Close() error { return nil }

func toolsWith(t *testing.T, provs ...tool.ToolProvider) *tool.ToolManager {
	t.Helper()
	tm := tool.NewToolManager(allowAllPerms{}, nil, nil)
	for _, p := range provs {
		if err := tm.AddProvider(p); err != nil {
			t.Fatalf("AddProvider(%s): %v", p.Name(), err)
		}
	}
	return tm
}

func validResearchHandoff() Handoff {
	return Handoff{
		Kind:   KindResearch,
		Status: "ok",
		Body: map[string]any{
			"findings":       "scope brief in research/research.md; schema in research/data-model.md",
			"memory_summary": "scoping complete",
			"file_refs":      []any{"research/data-model.md", "research/research.md"},
		},
	}
}

const (
	hookOK   = `{"exit_code":0,"stdout":"ok","stderr":""}`
	hookFail = `{"exit_code":2,"stdout":"","stderr":"data-model.md missing the Joins section"}`
)

// TestRunResearchStage_BeforeAndCheckHooks — the scaffold (before)
// hook fires once ahead of the loop; the gate (check) hook fires
// after each handoff and re-prompts the role on failure within the
// scaffold fires ONCE from the research stage; the CHECK hook is no
// longer called here (Phase 6.x moved it to the researcher's
// TurnFinalizeGate — gateResearchFinalize — so the file validation
// runs in-session). The researcher runs once and findings are stamped.
func TestRunResearchStage_ScaffoldFiresCheckMovedToGate(t *testing.T) {
	state := newRenderedFakeState("mis-r-hooks", productionRenderer(t))
	installMissionState(&state.fakeState)

	bash := &seqHookProvider{toolName: "bash:run", results: []json.RawMessage{json.RawMessage(hookOK)}}
	py := &seqHookProvider{toolName: "python:run_script", results: []json.RawMessage{json.RawMessage(hookOK)}}
	state.tools = toolsWith(t, bash, py)

	manifest := MissionManifest{
		Name:     "research-mission",
		Research: &ResearchManifest{Role: "researcher"},
		Stages: MissionStages{Research: StageHooks{
			Before: &MissionHook{Tool: "bash:run", Args: map[string]any{
				"command": "scaffold {{.MissionSkill}} into {{.MissionDir}}",
			}},
			Check: &MissionHook{Tool: "python:run_script"},
		}},
		SkillDir: "/skills/hub/analyst",
	}

	spawner := &plannerFakeSpawner{state: state}
	var spawns atomic.Int32
	spawner.onWorkerSpawn = func(_ SpawnRequest) Handoff {
		spawns.Add(1)
		return validResearchHandoff()
	}

	ext := newPlannerExtension()
	executor := NewExecutor(spawner.spawn, ext.logger)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	aborted, err := ext.runResearchStage(ctx, executor, state, manifest, manifest.Name, "build an HTML report")
	if err != nil {
		t.Fatalf("runResearchStage: %v", err)
	}
	if aborted {
		t.Fatalf("aborted = true, want false")
	}
	if got := bash.calls.Load(); got != 1 {
		t.Errorf("scaffold hook calls = %d, want 1 (fired once before the loop)", got)
	}
	if got := py.calls.Load(); got != 0 {
		t.Errorf("check hook calls = %d, want 0 (the check moved to gateResearchFinalize)", got)
	}
	if got := spawns.Load(); got != 1 {
		t.Errorf("researcher spawns = %d, want 1 (no in-stage re-prompt loop)", got)
	}
	if findings, _, _ := FromState(state).ResearchOutput(); findings == "" {
		t.Error("findings empty after research completed")
	}
}

// TestGateResearchFinalize covers the Option-B in-session gate: the
// researcher's turn is held open until both a parseable research fence
// AND a passing check hook. A failed check blocks (re-prompt
// continuation); a passing check allows. Phase 6.x — research→files.
func TestGateResearchFinalize(t *testing.T) {
	mission := newRenderedFakeState("mis-gate", productionRenderer(t))
	installMissionState(&mission.fakeState)
	FromState(mission).SetResearchRole("researcher")

	// Catalog returns a manifest with a check hook + a SkillDir.
	cat := NewStaticCatalog(&MissionManifest{
		Name:     "analyst",
		Plan:     MissionPlanManifest{Role: "planner"},
		SkillDir: "/skills/hub/analyst",
		Stages: MissionStages{Research: StageHooks{
			Check: &MissionHook{Tool: "bash:run"},
		}},
	})
	ext := newPlannerExtensionWithCatalog(cat)

	check := &seqHookProvider{toolName: "bash:run", results: []json.RawMessage{
		json.RawMessage(hookFail), json.RawMessage(hookOK),
	}}
	researcher := &renderedFakeState{renderer: productionRenderer(t)}
	researcher.id = "researcher-1"
	researcher.role = "researcher"
	researcher.skill = "analyst"
	researcher.parent = mission
	researcher.tools = toolsWith(t, check)

	fence := "```research\n{\"status\":\"ok\",\"body\":{\"findings\":\"x\"}}\n```"

	// 1st finalize: check fails → block (allow=false) + non-empty nudge.
	cont, allow := ext.GateTurnFinalize(context.Background(), researcher, fence)
	if allow {
		t.Fatalf("gate allowed finalize on a FAILED check; want block")
	}
	if cont == "" {
		t.Errorf("blocked gate returned empty continuation")
	}
	// 2nd finalize: check passes → allow.
	if _, allow := ext.GateTurnFinalize(context.Background(), researcher, fence); !allow {
		t.Errorf("gate blocked finalize on a PASSING check; want allow")
	}
	// A researcher that ended without a fence is blocked regardless.
	if _, allow := ext.GateTurnFinalize(context.Background(), researcher, ""); allow {
		t.Errorf("gate allowed an empty (fence-less) finalize; want block")
	}
}

// TestRunResearchStage_ScaffoldFailureAborts — a failing before-hook
// is fatal: the role never spawns and the stage aborts.
func TestRunResearchStage_ScaffoldFailureAborts(t *testing.T) {
	state := newRenderedFakeState("mis-r-scaffold-fail", productionRenderer(t))
	installMissionState(&state.fakeState)

	bash := &seqHookProvider{toolName: "bash:run", results: []json.RawMessage{json.RawMessage(hookFail)}}
	state.tools = toolsWith(t, bash)

	manifest := MissionManifest{
		Name:     "research-mission",
		Research: &ResearchManifest{Role: "researcher"},
		Stages: MissionStages{Research: StageHooks{
			Before: &MissionHook{Tool: "bash:run"},
		}},
	}

	spawner := &plannerFakeSpawner{state: state}
	var spawns atomic.Int32
	spawner.onWorkerSpawn = func(_ SpawnRequest) Handoff {
		spawns.Add(1)
		return validResearchHandoff()
	}

	ext := newPlannerExtension()
	executor := NewExecutor(spawner.spawn, ext.logger)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	aborted, err := ext.runResearchStage(ctx, executor, state, manifest, manifest.Name, "goal")
	if !aborted || err == nil {
		t.Fatalf("aborted=%v err=%v, want aborted=true with a scaffold error", aborted, err)
	}
	if got := spawns.Load(); got != 0 {
		t.Errorf("researcher spawns = %d, want 0 (scaffold gates the role)", got)
	}
}

// TestRunResearchStage_NoHooks_StillRuns — a research manifest with
// no stage hooks behaves exactly as before (backwards compatible);
// Tools() is never consulted.
func TestRunResearchStage_NoHooks_StillRuns(t *testing.T) {
	state := newRenderedFakeState("mis-r-nohooks", productionRenderer(t))
	installMissionState(&state.fakeState)
	// state.tools deliberately nil — the no-hook path must not touch it.

	manifest := MissionManifest{
		Name:     "research-mission",
		Research: &ResearchManifest{Role: "researcher"},
	}
	spawner := &plannerFakeSpawner{state: state}
	spawner.onWorkerSpawn = func(_ SpawnRequest) Handoff { return validResearchHandoff() }

	ext := newPlannerExtension()
	executor := NewExecutor(spawner.spawn, ext.logger)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	aborted, err := ext.runResearchStage(ctx, executor, state, manifest, manifest.Name, "goal")
	if err != nil || aborted {
		t.Fatalf("aborted=%v err=%v, want clean run with no hooks", aborted, err)
	}
}
