package fixture

import (
	"fmt"
	"os"
	"path/filepath"
)

// PlannerSkillName is the Phase-B fixture mission skill name —
// `_mission_v3` — exercising the planner-driven loop. Distinct
// from `_mission_v2` (Phase-A inline-plan fixture) so a single
// harness run can boot both side by side without renaming.
//
// The fixture exists to dogfood the iterative planner loop on a
// real LLM: the model spawned as `planner` reads the rendered
// planner_task template + goal, emits a kind=plan fence with one
// wave (echo worker), and on the second iteration emits
// next_wave: null to signal plan_complete. Synthesis then folds
// the wave's handoff into the mission's terminal answer.
//
// Approval is set to `skip` here so the scenario can run without
// a human inquiry responder. A separate fixture (Phase-B follow-up)
// can flip Initial back to `required` and pair with a scripted
// InquiryRule to exercise the approval gate end-to-end.
const PlannerSkillName = "_mission_v3"

// PlannerManifestYAML is the SKILL.md body the planner fixture
// writes. String-concatenation form (rather than a raw literal)
// because the inline worker task carries triple-backticks for the
// kind=plan / kind=handoff / kind=synthesis fences, which can't
// appear inside a Go raw string.
//
// The planner role gets a terse task brief — the heavy lifting
// (mission goal, iteration counter, approval directive, fence
// template) is added by mission ext via assets/prompts/mission/
// planner_task.tmpl. The role's on_start prose only needs to
// remind the model that it is the planner and that the fence
// instructions in the runtime-injected task body are authoritative.
const PlannerManifestYAML = "---\n" +
	"name: _mission_v3\n" +
	"description: Fixture — planner-driven PDCA mission with synthesizer.\n" +
	"license: Apache-2.0\n" +
	"allowed-tools:\n" +
	"  - provider: mission\n" +
	"    tools:\n" +
	"      - get_handoff\n" +
	"      - finish\n" +
	"metadata:\n" +
	"  hugen:\n" +
	"    tier_compatibility: [mission]\n" +
	"    sub_agents:\n" +
	"      - name: planner\n" +
	"        description: Emit the next wave for the mission, or signal plan_complete.\n" +
	"      - name: echo\n" +
	"        description: Echo the inputs back as a handoff fence.\n" +
	"      - name: synthesizer\n" +
	"        description: Combine prior handoffs into the mission's final answer.\n" +
	"    mission:\n" +
	"      summary: \"Phase B smoke-test mission — planner spawns a single echo wave then synthesises.\"\n" +
	"      plan:\n" +
	"        role: planner\n" +
	"        max_waves: 3\n" +
	"        approval:\n" +
	"          initial: skip\n" +
	"          iteration: never\n" +
	"      synthesis:\n" +
	"        role: synthesizer\n" +
	"compatibility:\n" +
	"  model: any\n" +
	"  runtime: hugen-phase-4\n" +
	"---\n" +
	"\n" +
	"# _mission_v3 fixture skill\n" +
	"\n" +
	"Phase B smoke-test mission. The planner role generates one wave\n" +
	"(a single `echo` worker) on iteration 1, then signals\n" +
	"plan_complete on iteration 2 (no further waves). The synthesizer\n" +
	"folds the echo worker's handoff into the mission's terminal\n" +
	"answer. Approval is disabled (`initial: skip`, `iteration: never`)\n" +
	"so dogfood runs don't require a human-in-the-loop responder.\n" +
	"Phase H deletes this fixture alongside the inline-plan escape\n" +
	"hatch once the analyst skill is migrated to the new shape.\n"

// WritePlannerTo writes the planner fixture under
// localSkillsRoot/<PlannerSkillName>/SKILL.md. Mirrors WriteTo for
// the Phase-A fixture; scenarios that exercise the planner loop
// call this in setup before runtime.Build.
func WritePlannerTo(localSkillsRoot string) error {
	dir := filepath.Join(localSkillsRoot, PlannerSkillName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mission planner fixture: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(PlannerManifestYAML), 0o600); err != nil {
		return fmt.Errorf("mission planner fixture: write %s: %w", path, err)
	}
	return nil
}
