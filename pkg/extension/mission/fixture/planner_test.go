package fixture

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hugr-lab/hugen/pkg/skill"
)

// TestPlannerManifestParses validates the Phase-B planner fixture
// YAML round-trips through skill.Parse with every mission-PDCA
// field populated (plan.role, approval, max_waves, synthesis).
func TestPlannerManifestParses(t *testing.T) {
	m, err := skill.Parse([]byte(PlannerManifestYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Name != PlannerSkillName {
		t.Errorf("Name = %q, want %q", m.Name, PlannerSkillName)
	}
	plan := m.Hugen.Mission.Plan
	if plan.Role != "planner" {
		t.Errorf("Plan.Role = %q, want planner", plan.Role)
	}
	if plan.MaxWaves != 3 {
		t.Errorf("Plan.MaxWaves = %d, want 3", plan.MaxWaves)
	}
	if plan.Approval.Initial != "skip" {
		t.Errorf("Plan.Approval.Initial = %q, want skip", plan.Approval.Initial)
	}
	if plan.Approval.Iteration != "never" {
		t.Errorf("Plan.Approval.Iteration = %q, want never", plan.Approval.Iteration)
	}
	if plan.Inline != nil {
		t.Errorf("Plan.Inline = %+v, want nil (planner-driven)", plan.Inline)
	}
	if m.Hugen.Mission.Synthesis.Role != "synthesizer" {
		t.Errorf("Synthesis.Role = %q, want synthesizer", m.Hugen.Mission.Synthesis.Role)
	}
	// sub_agents: planner + echo + synthesizer.
	if len(m.Hugen.SubAgents) != 3 {
		t.Errorf("sub_agents len = %d, want 3", len(m.Hugen.SubAgents))
	}
}

// TestWritePlannerToDropsSkillFile lays the planner fixture under
// a temp dir and re-parses to ensure on-disk shape matches the
// in-memory constant.
func TestWritePlannerToDropsSkillFile(t *testing.T) {
	root := t.TempDir()
	if err := WritePlannerTo(root); err != nil {
		t.Fatalf("WritePlannerTo: %v", err)
	}
	path := filepath.Join(root, PlannerSkillName, "SKILL.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written skill: %v", err)
	}
	if string(body) != PlannerManifestYAML {
		t.Fatal("written content differs from PlannerManifestYAML")
	}
	if _, err := skill.Parse(body); err != nil {
		t.Fatalf("Parse(written): %v", err)
	}
}
