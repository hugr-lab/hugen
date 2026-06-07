package mission

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
	wsext "github.com/hugr-lab/hugen/pkg/extension/workspace"
)

// specContractView is the typed payload the `mission/spec` template
// renders against. The top of the file is the immutable-ish contract
// (goal + AC); the Progress block below projects live PlanState
// (completed worker waves / the active wave / the planner's roadmap)
// so spec.md is a single durable snapshot of done/doing/planned.
// Phase 6.x — research→files; B39 — live snapshot.
type specContractView struct {
	Goal string
	AC   []specACView
	// Progress projects live PlanState directly (orchestration waves
	// filtered out in writeSpecContract). The template reads DoneWave
	// (Label / Status / Refs), Wave (Label + `len .Subagents`), and
	// RoadmapEntry (Label / Description) — no per-row reshape structs.
	// Phase B39.
	Done    []DoneWave
	Active  *Wave
	Roadmap []RoadmapEntry
}

// specACView is one acceptance-criterion row in spec.md. Done drives
// the `- [x]`/`- [ ]` checkbox (status==satisfied); Evidence is the
// criterion's LastEvidence prose (empty until first transition).
type specACView struct {
	ID        string
	Statement string
	Status    string
	Done      bool
	Evidence  string
}

// writeSpecContract projects the mission's committed contract (goal +
// AC roster) into `<mission_dir>/spec.md` so workers + the checker
// read a single durable contract file instead of relying on prompt-
// injected AC. Called from the validate_and_approve commit chokepoint
// on every approve path (same site as SetRoadmap), so spec.md tracks
// the latest approved contract.
//
// Best-effort: a session without a workspace dir (test fixtures) or
// prompts renderer is skipped silently; a render / write failure is
// logged and swallowed — the in-memory state.AC stays the source of
// truth, the file is a projection. The goal prefers the just-approved
// plan's restatement (currentMissionGoal is only stamped later, when
// the executor ingests the plan), falling back to the prior goal for
// the plan_complete (nil-plan) approve. Phase 6.x — research→files.
func (e *Extension) writeSpecContract(mission extension.SessionState, mState *MissionState, plan *Plan) {
	ws := wsext.FromState(mission)
	if ws == nil || ws.Dir() == "" {
		return
	}
	renderer := mission.Prompts()
	if renderer == nil {
		return
	}

	goal := ""
	if plan != nil {
		goal = strings.TrimSpace(plan.MissionGoal)
	}
	if goal == "" {
		goal, _, _ = mState.MissionFrame()
	}

	view := specContractView{Goal: goal}
	for _, ac := range mState.ACSnapshot() {
		if ac.Status == ACDropped {
			continue
		}
		view.AC = append(view.AC, specACView{
			ID:        ac.ID,
			Statement: ac.Statement,
			Status:    string(ac.Status),
			Done:      ac.Status == ACSatisfied,
			Evidence:  strings.TrimSpace(ac.LastEvidence),
		})
	}

	// Progress block — project live PlanState. Orchestration waves
	// (planner / checker / research / synthesis) are runtime
	// scaffolding, not user-facing work, so they are filtered out of
	// both the completed-wave list and the active marker.
	pstate := mState.PlanSnapshot()
	for _, d := range pstate.Done {
		if isOrchestrationWave(d.Label) {
			continue
		}
		view.Done = append(view.Done, d)
	}
	if pstate.Active != nil && !isOrchestrationWave(pstate.Active.Label) {
		view.Active = pstate.Active
	}
	view.Roadmap = pstate.Roadmap

	content, err := renderer.Render("mission/spec", view)
	if err != nil {
		e.logger.Warn("mission: render spec.md failed",
			"mission_session", mission.SessionID(), "err", err)
		return
	}
	// Content dirty-check + writer serialisation. spec.md is re-projected
	// on every wave transition (incl. orchestration waves that filter out
	// of the Progress block) plus the approve / checker chokepoints, so
	// most calls render byte-identical output — skip the write AND the
	// spec_written event when nothing changed (keeps the append-only event
	// log from accruing no-op rows). Holding specRenderMu across the write
	// also serialises the executor wave-hook goroutine and the planner
	// worker's validate_and_approve tool dispatch (no interleaved
	// os.WriteFile). Plan / checker writes still land — their content
	// genuinely changed. Phase B39.
	mState.specRenderMu.Lock()
	defer mState.specRenderMu.Unlock()
	if content == mState.lastSpecRender {
		return
	}
	path := filepath.Join(ws.Dir(), "spec.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		e.logger.Warn("mission: write spec.md failed",
			"mission_session", mission.SessionID(), "path", path, "err", err)
		return
	}
	mState.lastSpecRender = content
	e.emitMissionOp(mission, "spec_written",
		map[string]any{"path": "spec.md", "ac_count": len(view.AC)})
}

// snapshotSpec re-projects the mission's spec.md from current state.
// Wired as the executor's wave hook (WithWaveHook) so every wave
// transition — start and completion — refreshes the on-disk plan
// snapshot. Best-effort + nil-safe: a session without a MissionState
// is a no-op. Phase B39.
func (e *Extension) snapshotSpec(state extension.SessionState) {
	m := FromState(state)
	if m == nil {
		return
	}
	e.writeSpecContract(state, m, nil)
}
