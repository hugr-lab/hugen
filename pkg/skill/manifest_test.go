package skill

import (
	"errors"
	"strings"
	"testing"
)

func TestParse_HappyPath(t *testing.T) {
	src := `---
name: hello-world
description: A trivial skill that says hi.
license: MIT
compatibility:
  model: claude-sonnet-4
  runtime: hugen>=0.3.0
allowed-tools:
  - provider: bash-mcp
    tools:
      - bash.read_file
      - bash.write_file
metadata:
  hugen:
    requires: [_memory]
    intents: [demo]
---
# hello-world

Body here.
`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if m.Name != "hello-world" {
		t.Errorf("Name = %q, want hello-world", m.Name)
	}
	if m.License != "MIT" {
		t.Errorf("License = %q, want MIT", m.License)
	}
	if m.Compatibility.Model != "claude-sonnet-4" {
		t.Errorf("Compatibility.Model = %q", m.Compatibility.Model)
	}
	if len(m.AllowedTools) != 1 || m.AllowedTools[0].Provider != "bash-mcp" {
		t.Errorf("AllowedTools = %+v", m.AllowedTools)
	}
	if got := m.AllowedTools[0].Tools; len(got) != 2 {
		t.Errorf("AllowedTools[0].Tools len = %d, want 2", len(got))
	}
	if !contains(m.Hugen.Requires, "_memory") {
		t.Errorf("Hugen.Requires = %v, want to contain _memory", m.Hugen.Requires)
	}
	if !contains(m.Hugen.Intents, "demo") {
		t.Errorf("Hugen.Intents = %v, want to contain demo", m.Hugen.Intents)
	}
	if !strings.Contains(string(m.Body), "Body here") {
		t.Errorf("Body did not retain markdown content: %q", string(m.Body))
	}
}

func TestParse_FrontmatterOnly(t *testing.T) {
	src := `---
name: empty-body
description: Frontmatter only.
license: MIT
---
`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if m.Name != "empty-body" {
		t.Errorf("Name = %q", m.Name)
	}
	if got := strings.TrimSpace(string(m.Body)); got != "" {
		t.Errorf("Body = %q, want empty", got)
	}
}

func TestParse_NoFrontmatterRejected(t *testing.T) {
	src := `# A markdown file without frontmatter`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("Parse(no-frontmatter) returned nil error")
	}
	if !errors.Is(err, ErrManifestInvalid) {
		t.Errorf("err = %v, want ErrManifestInvalid", err)
	}
}

func TestParse_UnclosedFrontmatterRejected(t *testing.T) {
	src := `---
name: dangling
description: oops
`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("Parse(unclosed) returned nil error")
	}
	if !errors.Is(err, ErrManifestInvalid) {
		t.Errorf("err = %v, want ErrManifestInvalid", err)
	}
}

func TestParse_NameValidation(t *testing.T) {
	cases := []struct {
		name string
		want bool // true if Parse should succeed
	}{
		{"hello-world", true},
		{"_system", true},
		{"hello_world_123", true},
		{"PDF-Bad", true}, // mixed case is fine, agentskills.io spec only forbids charset/length
		{"with space", false},
		{"a/b", false},
		{"toolong-" + strings.Repeat("x", 64), false}, // > 64 chars
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "---\nname: " + tc.name + "\ndescription: x\nlicense: MIT\n---\n"
			_, err := Parse([]byte(src))
			if tc.want && err != nil {
				t.Errorf("Parse(%q) err = %v, want ok", tc.name, err)
			}
			if !tc.want && err == nil {
				t.Errorf("Parse(%q) err = nil, want failure", tc.name)
			}
		})
	}
}

func TestParse_MissingDescriptionRejected(t *testing.T) {
	src := `---
name: ok
license: MIT
---
`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("Parse(no-description) returned nil error")
	}
	if !errors.Is(err, ErrManifestInvalid) {
		t.Errorf("err = %v, want ErrManifestInvalid", err)
	}
}

func TestParse_DescriptionTooLong(t *testing.T) {
	src := "---\nname: ok\ndescription: " + strings.Repeat("x", 1025) + "\nlicense: MIT\n---\n"
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("Parse(too-long-description) returned nil error")
	}
}

func TestParse_AllowedToolsRequireProvider(t *testing.T) {
	src := `---
name: ok
description: x
license: MIT
allowed-tools:
  - tools: [foo.bar]
---
`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("Parse(no-provider) returned nil error")
	}
}

// TestParse_LegacySystemRenameHints asserts the validator points
// manifest authors at the post-step-25 owner when they still
// reference legacy `system:foo` tool names.
func TestParse_LegacySystemRenameHints(t *testing.T) {
	cases := []struct {
		legacyTool string
		newName    string
	}{
		{"notepad_append", "notepad:append"},
		{"skill_load", "session:skill_load"},
		{"policy_save", "policy:save"},
		{"mcp_add_server", "tool:provider_add"},
		{"runtime_reload", "runtime:reload"},
	}
	for _, tc := range cases {
		t.Run(tc.legacyTool, func(t *testing.T) {
			src := `---
name: legacy
description: refs an old name
license: MIT
allowed-tools:
  - provider: system
    tools: [` + tc.legacyTool + `]
---
`
			_, err := Parse([]byte(src))
			if err == nil {
				t.Fatalf("Parse: expected migration error for system:%s", tc.legacyTool)
			}
			if !strings.Contains(err.Error(), tc.newName) {
				t.Errorf("err = %v, want hint pointing at %q", err, tc.newName)
			}
		})
	}
}

func TestParse_UnknownTopLevelKeysPreserved(t *testing.T) {
	src := `---
name: ok
description: x
license: MIT
metadata:
  hugen:
    intents: [test]
  future_field:
    something: 42
---
`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if _, ok := m.Metadata["future_field"]; !ok {
		t.Errorf("Metadata.future_field missing — unknown keys must be preserved")
	}
	if !contains(m.Hugen.Intents, "test") {
		t.Errorf("Hugen.Intents not extracted from metadata.hugen.intents")
	}
}

func TestParse_BOMTolerant(t *testing.T) {
	bom := []byte{0xEF, 0xBB, 0xBF}
	src := append(bom, []byte("---\nname: ok\ndescription: x\nlicense: MIT\n---\n")...)
	m, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse with BOM error: %v", err)
	}
	if m.Name != "ok" {
		t.Errorf("Name = %q, want ok", m.Name)
	}
}

func TestParse_MalformedYAMLRejected(t *testing.T) {
	src := `---
name: [this is a list, not a string]
description: x
---
`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("Parse(malformed) returned nil error")
	}
	if !errors.Is(err, ErrManifestInvalid) {
		t.Errorf("err = %v, want ErrManifestInvalid", err)
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

// TestParse_RequiresSkillsCanonical exercises the phase-4 canonical
// `requires_skills` key. AllRequires must surface the dependency
// regardless of which spelling the manifest uses.
func TestParse_RequiresSkillsCanonical(t *testing.T) {
	src := `---
name: needs-planner
description: Sub-agent skill that pulls in the planner.
license: MIT
metadata:
  hugen:
    requires_skills:
      - _planner
---
`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !contains(m.Hugen.RequiresSkills, "_planner") {
		t.Errorf("Hugen.RequiresSkills = %v, want [_planner]", m.Hugen.RequiresSkills)
	}
	if !contains(m.Hugen.AllRequires(), "_planner") {
		t.Errorf("AllRequires = %v, want [_planner]", m.Hugen.AllRequires())
	}
}

// TestParse_RequiresAndRequiresSkills_Merged verifies a manifest
// using both spellings de-duplicates and preserves order
// (RequiresSkills first).
func TestParse_RequiresAndRequiresSkills_Merged(t *testing.T) {
	src := `---
name: dual
description: Both keys present.
license: MIT
metadata:
  hugen:
    requires_skills: [_planner, _memory]
    requires: [_memory, _system]
---
`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := m.Hugen.AllRequires()
	want := []string{"_planner", "_memory", "_system"}
	if len(got) != len(want) {
		t.Fatalf("AllRequires len = %d, want %d (got %v)", len(got), len(want), got)
	}
	for i, n := range want {
		if got[i] != n {
			t.Errorf("AllRequires[%d] = %q, want %q (full %v)", i, got[i], n, got)
		}
	}
}

// TestParse_PhaseFourFlags exercises the new manifest fields:
// max_turns_hard, stuck_detection, can_spawn, autoload_when_*.
func TestParse_PhaseFourFlags(t *testing.T) {
	src := `---
name: heavy-explorer
description: Phase-4 fields exercised end-to-end.
license: MIT
metadata:
  hugen:
    max_turns: 30
    max_turns_hard: 60
    stuck_detection:
      repeated_hash: 4
      tight_density_count: 5
      tight_density_window: "3s"
      enabled: false
    sub_agents:
      - name: explorer
        description: leaf
        can_spawn: false
    autoload: true
    autoload_for: [subagent]
    autoload_when_role_can_spawn: true
    autoload_when_parent_has_active_whiteboard: true
---
`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Hugen.MaxTurnsHard != 60 {
		t.Errorf("MaxTurnsHard = %d, want 60", m.Hugen.MaxTurnsHard)
	}
	if m.Hugen.StuckDetection.RepeatedHash != 4 {
		t.Errorf("StuckDetection.RepeatedHash = %d, want 4", m.Hugen.StuckDetection.RepeatedHash)
	}
	if m.Hugen.StuckDetection.IsEnabled() {
		t.Errorf("StuckDetection.IsEnabled = true, want false (explicit override)")
	}
	if len(m.Hugen.SubAgents) != 1 {
		t.Fatalf("SubAgents len = %d, want 1", len(m.Hugen.SubAgents))
	}
	if got := m.Hugen.SubAgents[0]; got.CanSpawnEffective() {
		t.Errorf("SubAgent.CanSpawnEffective = true, want false (explicit can_spawn: false)")
	}
	if !m.Hugen.AutoloadWhenRoleCanSpawn {
		t.Errorf("AutoloadWhenRoleCanSpawn not parsed")
	}
	if !m.Hugen.AutoloadWhenParentHasActiveWhiteboard {
		t.Errorf("AutoloadWhenParentHasActiveWhiteboard not parsed")
	}
}

// TestCanSpawn_DefaultsTrue verifies SubAgentRole.CanSpawnEffective
// returns true when the manifest omits can_spawn (the default-true
// semantic from §4.4).
func TestCanSpawn_DefaultsTrue(t *testing.T) {
	r := SubAgentRole{Name: "explorer"}
	if !r.CanSpawnEffective() {
		t.Error("CanSpawnEffective = false on default; want true")
	}
}

// TestStuckDetection_DefaultEnabled verifies IsEnabled returns true
// when Enabled is unset, mirroring the conservative-default stance
// from §8.3.
func TestStuckDetection_DefaultEnabled(t *testing.T) {
	var p StuckDetectionPolicy
	if !p.IsEnabled() {
		t.Error("IsEnabled = false on default; want true")
	}
}

// TestAutoloadEligible_RootIgnoresConditional verifies the
// conditional flags are silently ignored for root sessions —
// they target sub-agent autoload semantics by definition.
func TestAutoloadEligible_RootIgnoresConditional(t *testing.T) {
	m := &Manifest{}
	m.Hugen.Autoload = true
	m.Hugen.AutoloadWhenRoleCanSpawn = true
	if !m.AutoloadEligible(AutoloadContext{SessionType: SessionTypeRoot}) {
		t.Error("AutoloadEligible(root) = false; conditional flags should not apply to roots")
	}
}

// TestAutoloadEligible_SubAgent_RoleCanSpawnGate exercises the
// AutoloadWhenRoleCanSpawn gate: the manifest only autoloads when
// the spawned role's CanSpawn is true.
func TestAutoloadEligible_SubAgent_RoleCanSpawnGate(t *testing.T) {
	m := &Manifest{}
	m.Hugen.Autoload = true
	m.Hugen.AutoloadFor = []string{SessionTypeSubAgent}
	m.Hugen.AutoloadWhenRoleCanSpawn = true

	yes := AutoloadContext{SessionType: SessionTypeSubAgent, RoleCanSpawn: true}
	if !m.AutoloadEligible(yes) {
		t.Error("AutoloadEligible(role can spawn) = false, want true")
	}
	no := AutoloadContext{SessionType: SessionTypeSubAgent, RoleCanSpawn: false}
	if m.AutoloadEligible(no) {
		t.Error("AutoloadEligible(role cannot spawn) = true, want false")
	}
}

// TestAutoloadEligible_SubAgent_WhiteboardGate exercises the
// AutoloadWhenParentHasActiveWhiteboard gate.
func TestAutoloadEligible_SubAgent_WhiteboardGate(t *testing.T) {
	m := &Manifest{}
	m.Hugen.Autoload = true
	m.Hugen.AutoloadFor = []string{SessionTypeSubAgent}
	m.Hugen.AutoloadWhenParentHasActiveWhiteboard = true

	yes := AutoloadContext{SessionType: SessionTypeSubAgent, ParentHasActiveWhiteboard: true}
	if !m.AutoloadEligible(yes) {
		t.Error("AutoloadEligible(active whiteboard) = false, want true")
	}
	no := AutoloadContext{SessionType: SessionTypeSubAgent, ParentHasActiveWhiteboard: false}
	if m.AutoloadEligible(no) {
		t.Error("AutoloadEligible(no whiteboard) = true, want false")
	}
}
