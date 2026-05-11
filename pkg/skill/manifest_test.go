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
	// `compatibility:` is parsed leniently — the field has no
	// runtime effect (phase-4.1d removed Compatibility from the
	// Manifest struct since nothing in hugen reads it). Manifests
	// that still carry it should round-trip via Metadata without
	// erroring; the assertion lives in TestParse_PreservesUnknownFields.
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

// TestParse_PhaseFourFlags exercises the runtime manifest fields:
// max_turns_hard, stuck_detection, can_spawn, autoload_for /
// tier_compatibility (phase 4.2.2 tier vocab).
func TestParse_PhaseFourFlags(t *testing.T) {
	src := `---
name: _heavy-explorer
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
    autoload_for: [mission, worker]
    tier_compatibility: [mission, worker]
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

// TestAutoloadEligible covers the tier-only autoload decision
// after phase 4.2.2 removed the conditional gates. The simple
// rule: a tier in autoload_for fires; otherwise no.
func TestAutoloadEligible(t *testing.T) {
	m := &Manifest{}
	m.Hugen.Autoload = true
	m.Hugen.AutoloadFor = []string{TierMission, TierWorker}
	if !m.AutoloadEligible(TierMission) {
		t.Error("AutoloadEligible(mission) = false, want true")
	}
	if !m.AutoloadEligible(TierWorker) {
		t.Error("AutoloadEligible(worker) = false, want true")
	}
	if m.AutoloadEligible(TierRoot) {
		t.Error("AutoloadEligible(root) = true, want false (not in autoload_for)")
	}

	m.Hugen.Autoload = false
	if m.AutoloadEligible(TierMission) {
		t.Error("AutoloadEligible with autoload:false should always be false")
	}
}

// TestParse_AllowedTools_TriState verifies the load-bearing
// nil-vs-empty distinction on Manifest.AllowedTools that
// phase-4.2 §3.1 depends on. Three states must be
// distinguishable after Parse:
//   - absent (`allowed-tools` key missing) → nil slice.
//   - explicit empty (`allowed-tools: []`) → non-nil empty.
//   - populated → non-nil populated.
func TestParse_AllowedTools_TriState(t *testing.T) {
	cases := []struct {
		name      string
		src       string
		wantNil   bool
		wantLen   int
	}{
		{
			name: "absent",
			src: `---
name: absent-skill
description: no allowed-tools key.
license: MIT
---
`,
			wantNil: true,
			wantLen: 0,
		},
		{
			name: "explicit_empty",
			src: `---
name: empty-skill
description: explicit empty list.
license: MIT
allowed-tools: []
---
`,
			wantNil: false,
			wantLen: 0,
		},
		{
			name: "populated",
			src: `---
name: populated-skill
description: explicit grant.
license: MIT
allowed-tools:
  - bash-mcp:bash.run
---
`,
			wantNil: false,
			wantLen: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := Parse([]byte(tc.src))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			gotNil := m.AllowedTools == nil
			if gotNil != tc.wantNil {
				t.Errorf("AllowedTools nil = %v, want %v", gotNil, tc.wantNil)
			}
			if got := len(m.AllowedTools); got != tc.wantLen {
				t.Errorf("len(AllowedTools) = %d, want %d", got, tc.wantLen)
			}
		})
	}
}

// TestTierFromDepth covers the depth → tier mapping (phase 4.2.2
// §2). Negative depth maps to root (defensive — the constructor
// never produces negative depth, but the helper is robust).
func TestTierFromDepth(t *testing.T) {
	for _, tc := range []struct {
		depth int
		want  string
	}{
		{-1, TierRoot},
		{0, TierRoot},
		{1, TierMission},
		{2, TierWorker},
		{3, TierWorker},
		{10, TierWorker},
	} {
		if got := TierFromDepth(tc.depth); got != tc.want {
			t.Errorf("TierFromDepth(%d) = %q, want %q", tc.depth, got, tc.want)
		}
	}
}

// TestEffectiveTierCompatibility verifies the default-[worker]
// fallback when the manifest omits tier_compatibility (phase 4.2.2
// §3.3.2).
func TestEffectiveTierCompatibility(t *testing.T) {
	var m Manifest
	got := m.EffectiveTierCompatibility()
	if len(got) != 1 || got[0] != TierWorker {
		t.Errorf("EffectiveTierCompatibility absent = %v, want [%s]", got, TierWorker)
	}
	m.Hugen.TierCompatibility = []string{TierMission}
	got = m.EffectiveTierCompatibility()
	if len(got) != 1 || got[0] != TierMission {
		t.Errorf("EffectiveTierCompatibility explicit = %v, want [%s]", got, TierMission)
	}
}

// TestLoadableInTier covers tier_compatibility membership lookup
// including the absent-field default-[worker] fallback.
func TestLoadableInTier(t *testing.T) {
	var m Manifest
	if !m.LoadableInTier(TierWorker) {
		t.Errorf("absent tier_compatibility: LoadableInTier(worker) = false, want true (default)")
	}
	if m.LoadableInTier(TierRoot) {
		t.Errorf("absent tier_compatibility: LoadableInTier(root) = true, want false")
	}
	m.Hugen.TierCompatibility = []string{TierRoot, TierMission}
	if !m.LoadableInTier(TierRoot) || !m.LoadableInTier(TierMission) {
		t.Errorf("explicit [root,mission]: missing membership")
	}
	if m.LoadableInTier(TierWorker) {
		t.Errorf("explicit [root,mission]: LoadableInTier(worker) = true, want false")
	}
}

// TestParse_AutoloadRequiresUnderscorePrefix verifies the phase
// 4.2.2 §1 invariant: autoload:true is reserved for system skills
// (name must start with "_"). The error must satisfy
// errors.Is(err, ErrAutoloadReserved) so handlers can recover the
// sentinel for user-friendly messaging.
func TestParse_AutoloadRequiresUnderscorePrefix(t *testing.T) {
	src := `---
name: community-skill
description: Community-authored skill trying to claim autoload.
license: MIT
metadata:
  hugen:
    autoload: true
    autoload_for: [worker]
    tier_compatibility: [worker]
---
`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("Parse: nil err, want ErrAutoloadReserved")
	}
	if !errors.Is(err, ErrAutoloadReserved) {
		t.Errorf("err = %v, want errors.Is ErrAutoloadReserved", err)
	}
	if !errors.Is(err, ErrManifestInvalid) {
		t.Errorf("err = %v, want errors.Is ErrManifestInvalid (parse-time)", err)
	}
}

// TestParse_AutoloadRequiresExplicitAutoloadFor verifies the
// phase 4.2.2 §3 invariant: when autoload:true the manifest must
// declare autoload_for explicitly — no [root] fallback.
func TestParse_AutoloadRequiresExplicitAutoloadFor(t *testing.T) {
	src := `---
name: _ghost
description: Autoload without an explicit autoload_for.
license: MIT
metadata:
  hugen:
    autoload: true
    tier_compatibility: [root]
---
`
	_, err := Parse([]byte(src))
	if err == nil || !errors.Is(err, ErrManifestInvalid) {
		t.Fatalf("Parse: err = %v, want ErrManifestInvalid", err)
	}
	if !strings.Contains(err.Error(), "autoload_for") {
		t.Errorf("err message should reference autoload_for: %v", err)
	}
}

// TestParse_AutoloadForSubsetOfTierCompatibility verifies the
// invariant autoload_for ⊆ tier_compatibility. A skill that
// declares autoload_for:[root] but tier_compatibility:[worker]
// would auto-load where skill:load would reject it — caught at
// parse time.
func TestParse_AutoloadForSubsetOfTierCompatibility(t *testing.T) {
	src := `---
name: _mismatch
description: autoload_for not subset of tier_compatibility.
license: MIT
metadata:
  hugen:
    autoload: true
    autoload_for: [root]
    tier_compatibility: [worker]
---
`
	_, err := Parse([]byte(src))
	if err == nil || !errors.Is(err, ErrManifestInvalid) {
		t.Fatalf("Parse: err = %v, want ErrManifestInvalid", err)
	}
	if !strings.Contains(err.Error(), "tier_compatibility") {
		t.Errorf("err message should mention tier_compatibility subset: %v", err)
	}
}

// TestParse_RejectsLegacyTierVocab verifies the aggressive cleanup
// per phase 4.2.2 §Migration: the legacy [subagent] alias is gone
// — autoload_for must use the new [root, mission, worker] vocab.
func TestParse_RejectsLegacyTierVocab(t *testing.T) {
	src := `---
name: _legacy
description: Uses the dropped [subagent] vocabulary.
license: MIT
metadata:
  hugen:
    autoload: true
    autoload_for: [subagent]
    tier_compatibility: [subagent]
---
`
	_, err := Parse([]byte(src))
	if err == nil || !errors.Is(err, ErrManifestInvalid) {
		t.Fatalf("Parse: err = %v, want ErrManifestInvalid", err)
	}
}

// TestParse_RejectsInvalidTierEntry verifies tier_compatibility
// entries are checked against {root, mission, worker}.
func TestParse_RejectsInvalidTierEntry(t *testing.T) {
	src := `---
name: _bad-tier
description: tier_compatibility has an unknown value.
license: MIT
metadata:
  hugen:
    tier_compatibility: [shaman]
---
`
	_, err := Parse([]byte(src))
	if err == nil || !errors.Is(err, ErrManifestInvalid) {
		t.Fatalf("Parse: err = %v, want ErrManifestInvalid", err)
	}
	if !strings.Contains(err.Error(), "shaman") {
		t.Errorf("err should name the offending value: %v", err)
	}
}

// TestParse_TierCompatibilityValidAll exercises the happy path
// for every tier value plus the absent-field default-[worker]
// fallback at parse time.
func TestParse_TierCompatibilityValidAll(t *testing.T) {
	src := `---
name: _allgood
description: tier_compatibility uses every tier value.
license: MIT
metadata:
  hugen:
    autoload: true
    autoload_for: [root, mission, worker]
    tier_compatibility: [root, mission, worker]
---
`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(m.Hugen.TierCompatibility) != 3 {
		t.Errorf("TierCompatibility = %v, want 3 entries", m.Hugen.TierCompatibility)
	}
}
