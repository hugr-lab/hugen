package mission

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
	wsext "github.com/hugr-lab/hugen/pkg/extension/workspace"
)

// specContractView is the typed payload the `mission/spec` template
// renders against — the committed goal + the non-dropped acceptance
// criteria roster (id + statement + status). Phase 6.x —
// research→files.
type specContractView struct {
	Goal string
	AC   []specACView
}

// specACView is one acceptance-criterion row in spec.md. Status is
// the raw ACStatus string ("unsatisfied" / "satisfied").
type specACView struct {
	ID        string
	Statement string
	Status    string
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
		})
	}

	content, err := renderer.Render("mission/spec", view)
	if err != nil {
		e.logger.Warn("mission: render spec.md failed",
			"mission_session", mission.SessionID(), "err", err)
		return
	}
	path := filepath.Join(ws.Dir(), "spec.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		e.logger.Warn("mission: write spec.md failed",
			"mission_session", mission.SessionID(), "path", path, "err", err)
		return
	}
	e.emitMissionOp(mission, "spec_written",
		map[string]any{"path": "spec.md", "ac_count": len(view.AC)})
}
