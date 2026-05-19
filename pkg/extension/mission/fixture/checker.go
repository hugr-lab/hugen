package fixture

import (
	"fmt"
	"os"
	"path/filepath"
)

// CheckerSkillName is the Phase-C/D/E fixture mission skill name —
// `_mission_v4` — exercising the full PDCA loop: planner role +
// checker role + synthesizer role with memory_summary entries
// feeding into the plan_context journal.
//
// Approval is set to `skip` so the scenario stays headless. The
// checker is expected to emit decision=continue after the first
// wave and finish (or let the planner emit plan_complete) after
// the second iteration's wave settles.
const CheckerSkillName = "_mission_v4"

// CheckerManifestYAML is the SKILL.md body the planner+checker
// fixture writes. Matches the _mission_v3 shape with an added
// `control.role: checker` block + a `checker` sub_agent role.
const CheckerManifestYAML = "---\n" +
	"name: _mission_v4\n" +
	"description: Phase C/D/E fixture — full PDCA mission with planner + checker + synthesizer. Deleted at Phase H end alongside the experimental_inline escape hatch.\n" +
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
	"        capabilities:\n" +
	"          plan_context: read\n" +
	"      - name: checker\n" +
	"        description: Inspect the prior wave's handoffs and emit a verdict (continue / amend / inquire / finish).\n" +
	"        capabilities:\n" +
	"          plan_context: read\n" +
	"      - name: echo\n" +
	"        description: Echo the inputs back as a handoff fence.\n" +
	"        capabilities:\n" +
	"          plan_context: off\n" +
	"      - name: synthesizer\n" +
	"        description: Combine prior handoffs into the mission's final answer.\n" +
	"        capabilities:\n" +
	"          plan_context: read\n" +
	"    mission:\n" +
	"      summary: \"Phase C/D/E smoke-test mission — planner + checker + synthesis loop.\"\n" +
	"      capabilities:\n" +
	"        notepad: true\n" +
	"        whiteboard: false\n" +
	"        plan_context: true\n" +
	"      plan:\n" +
	"        role: planner\n" +
	"        max_waves: 3\n" +
	"        approval:\n" +
	"          initial: skip\n" +
	"          iteration: never\n" +
	"      control:\n" +
	"        role: checker\n" +
	"      synthesis:\n" +
	"        role: synthesizer\n" +
	"compatibility:\n" +
	"  model: any\n" +
	"  runtime: hugen-phase-4\n" +
	"---\n" +
	"\n" +
	"# _mission_v4 fixture skill\n" +
	"\n" +
	"Phase C/D/E smoke-test mission. Exercises the full PDCA loop:\n" +
	"\n" +
	"1. Planner emits a kind=plan handoff with the next wave + a\n" +
	"   memory_summary describing the planning rationale.\n" +
	"2. Executor runs the planner-emitted wave (one echo worker).\n" +
	"3. Checker inspects the worker's handoff and emits a kind=verdict\n" +
	"   handoff with `decision: continue` or `decision: finish`.\n" +
	"4. Planner sees the prior handoffs + verdict + plan_context\n" +
	"   journal entries on its next spawn; signals plan_complete.\n" +
	"5. Synthesizer folds the wave's handoff into the mission's\n" +
	"   terminal answer.\n" +
	"\n" +
	"Approval is disabled (`initial: skip`, `iteration: never`) so\n" +
	"the scenario runs headless. Phase H deletes this fixture.\n"

// WriteCheckerTo writes the planner+checker fixture under
// localSkillsRoot/<CheckerSkillName>/SKILL.md.
func WriteCheckerTo(localSkillsRoot string) error {
	dir := filepath.Join(localSkillsRoot, CheckerSkillName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mission checker fixture: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(CheckerManifestYAML), 0o600); err != nil {
		return fmt.Errorf("mission checker fixture: write %s: %w", path, err)
	}
	return nil
}
