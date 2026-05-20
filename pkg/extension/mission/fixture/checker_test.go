package fixture

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hugr-lab/hugen/pkg/skill"
)

func TestCheckerManifestParses(t *testing.T) {
	m, err := skill.Parse([]byte(CheckerManifestYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Name != CheckerSkillName {
		t.Errorf("Name = %q, want %q", m.Name, CheckerSkillName)
	}
	if m.Hugen.Mission.Plan.Role != "planner" {
		t.Errorf("Plan.Role = %q, want planner", m.Hugen.Mission.Plan.Role)
	}
	if m.Hugen.Mission.Control.Role != "checker" {
		t.Errorf("Control.Role = %q, want checker", m.Hugen.Mission.Control.Role)
	}
	if m.Hugen.Mission.Synthesis.Role != "synthesizer" {
		t.Errorf("Synthesis.Role = %q, want synthesizer", m.Hugen.Mission.Synthesis.Role)
	}
	// sub_agents: planner + checker + echo + synthesizer.
	if len(m.Hugen.SubAgents) != 4 {
		t.Errorf("sub_agents len = %d, want 4", len(m.Hugen.SubAgents))
	}
}

func TestWriteCheckerToDropsSkillFile(t *testing.T) {
	root := t.TempDir()
	if err := WriteCheckerTo(root); err != nil {
		t.Fatalf("WriteCheckerTo: %v", err)
	}
	path := filepath.Join(root, CheckerSkillName, "SKILL.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written: %v", err)
	}
	if string(body) != CheckerManifestYAML {
		t.Fatal("written content differs")
	}
	if _, err := skill.Parse(body); err != nil {
		t.Fatalf("Parse(written): %v", err)
	}
}
