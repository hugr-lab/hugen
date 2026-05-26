package fixture

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hugr-lab/hugen/pkg/skill"
)

// TestManifestParses validates the fixture YAML round-trips through
// the canonical skill.Parse path with every field mission ext relies
// on populated.
func TestManifestParses(t *testing.T) {
	m, err := skill.Parse([]byte(ManifestYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Name != SkillName {
		t.Errorf("Name = %q, want %q", m.Name, SkillName)
	}
	plan := m.Hugen.Mission.Plan.Inline
	if plan == nil {
		t.Fatal("Hugen.Mission.Plan.Inline is nil")
	}
	if len(plan.Waves) != 1 {
		t.Fatalf("Plan.Inline.Waves len = %d, want 1", len(plan.Waves))
	}
	w := plan.Waves[0]
	if w.Label != "wave-1" {
		t.Errorf("wave[0].Label = %q, want wave-1", w.Label)
	}
	if len(w.Subagents) != 1 {
		t.Fatalf("wave[0].Subagents len = %d, want 1", len(w.Subagents))
	}
	sa := w.Subagents[0]
	if sa.Name != "w1" || sa.Role != "echo" {
		t.Errorf("subagent[0] = %+v, want Name=w1 Role=echo", sa)
	}
	if m.Hugen.Mission.Synthesis.Role != "synthesizer" {
		t.Errorf("Synthesis.Role = %q, want synthesizer", m.Hugen.Mission.Synthesis.Role)
	}
}

// TestWriteToDropsSkillFile creates the fixture under a temp dir
// and re-parses the resulting SKILL.md via skill.Parse to ensure
// the on-disk shape is byte-identical to the in-memory constant.
func TestWriteToDropsSkillFile(t *testing.T) {
	root := t.TempDir()
	if err := WriteTo(root); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	path := filepath.Join(root, SkillName, "SKILL.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written skill: %v", err)
	}
	if string(body) != ManifestYAML {
		t.Fatalf("written content differs from ManifestYAML")
	}
	m, err := skill.Parse(body)
	if err != nil {
		t.Fatalf("Parse(written): %v", err)
	}
	if m.Name != SkillName {
		t.Errorf("parsed Name = %q, want %q", m.Name, SkillName)
	}
}
