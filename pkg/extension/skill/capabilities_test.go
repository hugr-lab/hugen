package skill

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	skillpkg "github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// Manifest carrying a legacy on_tool_error hint (parse normalises it to
// on_tool_result) AND a native on_tool_result hint, used by the
// OnToolResult advisor tests.
const inlineHintManifest = `---
name: hinted
description: A skill with in-turn tool hints.
license: MIT
metadata:
  hugen:
    tier_compatibility: [root, mission, worker]
    hints:
      - type: on_tool_error
        tools: ["hugr-main:data-*"]
        match: "Cannot query field .*_aggregation"
        message: "Enumerate modules via discovery-search_modules first."
      - type: on_tool_result
        tools: ["hugr-main:data-inline_graphql_result"]
        match: 'is_truncated"?\s*:\s*true'
        message: "Truncated — switch to hugr-query file output."
---
hinted body
`

// Manifest used by FilterTools tests: grants a wildcard discovery-*
// + schema-* on hugr-main and an exact `query` on hugr-query.
const inlineDatasetManifest = `---
name: dataset
description: minimal skill granting wildcards.
allowed-tools:
  - provider: hugr-main
    tools:
      - discovery-*
      - schema-*
  - provider: hugr-query
    tools:
      - query
compatibility:
  model: any
  runtime: hugen-phase-3
---
body
`

func TestFilterTools_WildcardAndExact(t *testing.T) {
	ctx := context.Background()
	store := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{"dataset": []byte(inlineDatasetManifest)}})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "agent-test")
	state := fixture.NewTestSessionState("ses-fil").WithDepth(2)
	if err := ext.InitState(ctx, state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	if err := FromState(state).Load(ctx, "dataset"); err != nil {
		t.Fatalf("Load: %v", err)
	}

	in := []tool.Tool{
		{Name: "hugr-main:discovery-search_modules", Provider: "hugr-main"},
		{Name: "hugr-main:discovery-search_data_sources", Provider: "hugr-main"},
		{Name: "hugr-main:schema-type_fields", Provider: "hugr-main"},
		{Name: "hugr-main:data-validate_graphql_query", Provider: "hugr-main"},
		{Name: "hugr-query:query", Provider: "hugr-query"},
		{Name: "hugr-query:query_jq", Provider: "hugr-query"},
	}

	got := map[string]bool{}
	for _, t := range ext.FilterTools(ctx, state, in) {
		got[t.Name] = true
	}

	want := []string{
		"hugr-main:discovery-search_modules",
		"hugr-main:discovery-search_data_sources",
		"hugr-main:schema-type_fields",
		"hugr-query:query",
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing tool %q in filtered set; got %v", w, got)
		}
	}
	if got["hugr-main:data-validate_graphql_query"] {
		t.Errorf("data-validate_graphql_query leaked — wildcard scope is wrong")
	}
	if got["hugr-query:query_jq"] {
		t.Errorf("query_jq leaked — exact-match grant should not include siblings")
	}
}

// TestFilterTools_RequiresApprovalTagging verifies the phase-5.1
// § η flag plumbing: skill manifests carrying requires_approval
// entries (exact + '*' wildcard) project the flag onto the
// per-session snapshot's tool.Tool. The dispatcher reads
// t.RequiresApproval to decide whether to interpose an inquire
// flow before forwarding to the provider.
func TestFilterTools_RequiresApprovalTagging(t *testing.T) {
	const inline = `---
name: gated
description: minimal skill exercising requires_approval matching.
allowed-tools:
  - provider: bash-mcp
    tools:
      - bash.run
      - bash.shell
      - bash.read_file
    requires_approval:
      - bash.run
      - bash.shell
  - provider: hugr-main
    tools:
      - data-*
    requires_approval: ['*']
compatibility:
  model: any
  runtime: hugen-phase-3
---
body
`
	ctx := context.Background()
	store := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{"gated": []byte(inline)}})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "agent-test")
	state := fixture.NewTestSessionState("ses-app").WithDepth(2)
	if err := ext.InitState(ctx, state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	if err := FromState(state).Load(ctx, "gated"); err != nil {
		t.Fatalf("Load: %v", err)
	}

	in := []tool.Tool{
		{Name: "bash-mcp:bash.run", Provider: "bash-mcp"},
		{Name: "bash-mcp:bash.shell", Provider: "bash-mcp"},
		{Name: "bash-mcp:bash.read_file", Provider: "bash-mcp"},
		{Name: "hugr-main:data-inline_graphql_result", Provider: "hugr-main"},
	}
	out := ext.FilterTools(ctx, state, in)

	flags := map[string]bool{}
	for _, t := range out {
		flags[t.Name] = t.RequiresApproval
	}
	want := map[string]bool{
		"bash-mcp:bash.run":                    true,
		"bash-mcp:bash.shell":                  true,
		"bash-mcp:bash.read_file":              false,
		"hugr-main:data-inline_graphql_result": true,
	}
	for name, want := range want {
		got, present := flags[name]
		if !present {
			t.Errorf("tool %q dropped from filtered set", name)
			continue
		}
		if got != want {
			t.Errorf("tool %q RequiresApproval = %v, want %v", name, got, want)
		}
	}
}

// nil SkillManager → no filter (every tool surfaces).
func TestFilterTools_NilManagerExposesEverything(t *testing.T) {
	ext := NewExtension(nil, nil, "a1")
	state := fixture.NewTestSessionState("ses-nil").WithDepth(2)
	in := []tool.Tool{{Name: "a:x", Provider: "a"}, {Name: "b:y", Provider: "b"}}
	out := ext.FilterTools(context.Background(), state, in)
	if len(out) != 2 {
		t.Errorf("nil manager dropped tools: got %v", out)
	}
}

// Skill manager wired but no skill loaded → empty filtered set
// (distinguishes "no manager" from "no skills loaded"; mirrors
// the pre-stage-2 applySkillFilter behaviour).
func TestFilterTools_NoSkillLoadedYieldsEmpty(t *testing.T) {
	store := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{}})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "a1")
	state := fixture.NewTestSessionState("ses-empty").WithDepth(2)
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	in := []tool.Tool{{Name: "a:x", Provider: "a"}}
	out := ext.FilterTools(context.Background(), state, in)
	if len(out) != 0 {
		t.Errorf("expected empty catalogue, got %v", out)
	}
}

func TestAllowedSet_Match(t *testing.T) {
	a := &allowedSet{
		exact:    map[string]bool{"a:b": true},
		patterns: []string{"a:p"},
	}
	for _, c := range []struct {
		name string
		want bool
	}{
		{"a:b", true},
		{"a:p", true},
		{"a:px", true},
		{"a:other", false},
		{"b:p", false},
	} {
		if got := a.match(c.name); got != c.want {
			t.Errorf("match(%q) = %v, want %v", c.name, got, c.want)
		}
	}
	var n *allowedSet
	if !n.match("anything") {
		t.Errorf("nil allowedSet should match unconditionally")
	}
}

// Generation pulls the SkillManager bindings generation for the
// state's session, and bumps on Load / Unload.
func TestGeneration_BumpsOnLoadUnload(t *testing.T) {
	ctx := context.Background()
	store := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{"alpha": []byte(inlineAlphaManifest)}})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "a1")
	state := fixture.NewTestSessionState("ses-gen").WithDepth(2)
	if err := ext.InitState(ctx, state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	g0 := ext.Generation(state)
	if err := FromState(state).Load(ctx, "alpha"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	g1 := ext.Generation(state)
	if g1 <= g0 {
		t.Errorf("Generation did not bump on Load: %d → %d", g0, g1)
	}
	if err := FromState(state).Unload(ctx, "alpha"); err != nil {
		t.Fatalf("Unload: %v", err)
	}
	g2 := ext.Generation(state)
	if g2 <= g1 {
		t.Errorf("Generation did not bump on Unload: %d → %d", g1, g2)
	}
}

// TestAdvertiseSystemPrompt_AvailableMissions_MovedToMissionExt
// — placeholder for the deleted coverage: the "## Available
// missions" block is now owned by pkg/extension/mission's
// Advertiser, not skill ext. The new path is exercised in
// pkg/extension/mission/dispatcher_test.go. Mission-PDCA
// (design 003) — no fallback.

// TestTurnPreamble_NotepadTagsBlockA verifies the Block A section
// ("## Notepad — recommended tags") renders in the turn_preamble — B31
// moved it off the system prompt so it rides next to the catalogue +
// the notepad snapshot past the KV-cache boundary — when a loaded skill
// declares top-level metadata.hugen.notepad.tags, and that it is ABSENT
// from the system prompt.
func TestTurnPreamble_NotepadTagsBlockA(t *testing.T) {
	ctx := context.Background()
	skillWithTags := `---
name: analyst
description: data analysis skill.
metadata:
  hugen:
    tier_compatibility: [mission]
    notepad:
      tags:
        - name: schema-finding
          hint: Discovered table structures or field semantics.
        - name: data-quality-issue
          hint: Anomalies, nulls, suspicious cardinalities.
        - name: deferred-question
    mission:
      enabled: true
      summary: Data analysis, queries, reports.
---
body
`
	store := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{
		"analyst": []byte(skillWithTags),
	}})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "a1")

	missState := fixture.NewTestSessionState("ses-m").WithDepth(1)
	if err := ext.InitState(ctx, missState); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	if err := FromState(missState).Load(ctx, "analyst"); err != nil {
		t.Fatalf("Load: %v", err)
	}

	pre := ext.TurnPreamble(ctx, missState)
	for _, want := range []string{
		"## Notepad — recommended tags",
		"`schema-finding`",
		"Discovered table structures",
		"`data-quality-issue`",
		"`deferred-question`",
	} {
		if !strings.Contains(pre, want) {
			t.Errorf("Block A missing %q from turn_preamble; got:\n%s", want, pre)
		}
	}
	if sys := ext.AdvertiseSystemPrompt(ctx, missState); strings.Contains(sys, "## Notepad — recommended tags") {
		t.Errorf("Block A must NOT appear in the system prompt after B31; got:\n%s", sys)
	}
}

// TestTurnPreamble_NotepadTagsEmptyWhenNoMissionSkill — Block A omits
// entirely when no loaded skill declares notepad tags (e.g. workers, or
// missions whose dispatcher doesn't use the field). B31 — the tags now
// render in the turn_preamble, so that's where their absence is checked.
func TestTurnPreamble_NotepadTagsEmptyWhenNoMissionSkill(t *testing.T) {
	ctx := context.Background()
	noTags := `---
name: analyst
description: bare mission skill.
metadata:
  hugen:
    tier_compatibility: [mission]
    mission:
      enabled: true
      summary: bare.
---
body
`
	store := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{
		"analyst": []byte(noTags),
	}})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "a1")

	missState := fixture.NewTestSessionState("ses-m").WithDepth(1)
	if err := ext.InitState(ctx, missState); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	if err := FromState(missState).Load(ctx, "analyst"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	out := ext.TurnPreamble(ctx, missState)
	if strings.Contains(out, "## Notepad — recommended tags") {
		t.Errorf("expected no Block A when no tags declared; got:\n%s", out)
	}
}

// TestAdvertiseSystemPrompt_NoMissionsSkipsBlock verifies the
// section is omitted entirely when no installed skill declares
// mission.enabled.
func TestAdvertiseSystemPrompt_NoMissionsSkipsBlock(t *testing.T) {
	ctx := context.Background()
	store := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{
		"alpha": []byte(inlineAlphaManifest),
	}})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "a1")
	rootState := fixture.NewTestSessionState("ses-root2").WithDepth(0)
	if err := ext.InitState(ctx, rootState); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	out := ext.AdvertiseSystemPrompt(ctx, rootState)
	if strings.Contains(out, "## Available missions") {
		t.Errorf("Available missions rendered with no mission.enabled skills: %s", out)
	}
}

// Phase 6.x — the AVAILABLE-skills catalogue migrated from the system
// prompt (AdvertiseSystemPrompt) to the ModelInTurnAdvisor
// turn_preamble. With no skill loaded only the catalogue surfaces, and
// it now comes from TurnPreamble.
func TestTurnPreamble_CatalogueOnly(t *testing.T) {
	ctx := context.Background()
	store := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{"alpha": []byte(inlineAlphaManifest)}})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "a1")
	state := fixture.NewTestSessionState("ses-adv").WithDepth(2)
	if err := ext.InitState(ctx, state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	// Catalogue no longer baked into the system prompt.
	if sys := ext.AdvertiseSystemPrompt(ctx, state); strings.Contains(sys, "## Available skills") {
		t.Errorf("catalogue leaked into system prompt: %s", sys)
	}
	out := ext.TurnPreamble(ctx, state)
	if !strings.Contains(out, "## Available skills") {
		t.Errorf("missing catalogue heading: %s", out)
	}
	if !strings.Contains(out, "`alpha`") {
		t.Errorf("alpha bullet missing: %s", out)
	}
	if strings.Contains(out, "`alpha` (loaded)") {
		t.Errorf("nothing loaded yet, but alpha tagged loaded: %s", out)
	}
}

// TestTurnPreamble_TaskEligibleSkillsHidden verifies the catalogue
// split: skills with `task.eligible: true` do NOT appear in the
// `## Available skills` catalogue (reserved for loadable category /
// utility skills) — they surface in the parallel `## Available tasks`
// block instead (B47 step 5), runnable via task:execute_task.
func TestTurnPreamble_TaskEligibleSkillsHidden(t *testing.T) {
	ctx := context.Background()
	store := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{
		"alpha": []byte(inlineAlphaManifest),
		"data_tables_rows_count": []byte(`---
name: data_tables_rows_count
description: A recipe.
license: MIT
metadata:
  hugen:
    tier_compatibility: [worker]
    task: {eligible: true, kind: worker}
---
`),
	}})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "a1")
	state := fixture.NewTestSessionState("ses-adv-recipes").WithDepth(2)
	if err := ext.InitState(ctx, state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	out := ext.TurnPreamble(ctx, state)
	if !strings.Contains(out, "`alpha`") {
		t.Errorf("non-recipe alpha must still appear:\n%s", out)
	}
	// Split at the tasks header: the recipe must be absent from the skills
	// section but present in the tasks section.
	skillsSection, tasksSection, split := strings.Cut(out, "## Available tasks")
	if !split {
		t.Fatalf("expected an `## Available tasks` block:\n%s", out)
	}
	if strings.Contains(skillsSection, "data_tables_rows_count") {
		t.Errorf("task-eligible recipe leaked into Available skills:\n%s", skillsSection)
	}
	if !strings.Contains(tasksSection, "data_tables_rows_count") {
		t.Errorf("task-eligible recipe missing from Available tasks:\n%s", tasksSection)
	}
}

// TestReportStatus_TaskCatalogueTokens verifies the `## Available
// tasks` advertise block is metered separately as
// `available_task_tokens`, split out of the skills catalogue so the
// context-budget UI shows the task menu's cost on its own line.
func TestReportStatus_TaskCatalogueTokens(t *testing.T) {
	ctx := context.Background()
	store := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{
		"alpha":   []byte(inlineAlphaManifest),                                          // normal skill → `## Available skills`
		"roadrep": []byte(taskManifest("roadrep", []string{"bash-mcp:bash.read_file"})), // task-eligible → `## Available tasks`
	}})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "a1")
	state := fixture.NewTestSessionState("ses-task-budget").WithDepth(0) // root sees the task menu
	if err := ext.InitState(ctx, state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	_ = ext.AdvertiseSystemPrompt(ctx, state)
	_ = ext.TurnPreamble(ctx, state)
	raw := ext.ReportStatus(ctx, state)
	if raw == nil {
		t.Fatalf("ReportStatus = nil")
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	taskTok, ok := body["available_task_tokens"].(float64)
	if !ok || taskTok <= 0 {
		t.Fatalf("available_task_tokens missing/zero with a task-eligible skill in the catalogue: %v", body)
	}
	// The skills catalogue (alpha) is counted separately and must NOT
	// fold in the task block.
	if skillTok, ok := body["available_skill_tokens"].(float64); !ok || skillTok <= 0 {
		t.Errorf("available_skill_tokens should be >0 (alpha) and separate from tasks: %v", body)
	}
}

// TestReportStatus_AdvertiseSplit_LoadedVsCatalogue verifies the
// γ split: ReportStatus surfaces `loaded_skill_tokens` and
// `available_skill_tokens` separately so the context-budget UI
// can show "you've loaded N kB of skill bodies; the catalogue
// itself costs another M kB". A catalogue-only session has zero
// loaded tokens; loading a skill grows the loaded side.
func TestReportStatus_AdvertiseSplit_LoadedVsCatalogue(t *testing.T) {
	ctx := context.Background()
	store := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{"alpha": []byte(inlineAlphaManifest)}})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "a1")
	state := fixture.NewTestSessionState("ses-split").WithDepth(2)
	if err := ext.InitState(ctx, state); err != nil {
		t.Fatalf("InitState: %v", err)
	}

	// First render — no skill loaded yet. The loaded side
	// (AdvertiseSystemPrompt) stays empty; the catalogue advertises
	// alpha via the turn_preamble. Phase 6.x splits the render across
	// both capabilities, so both must run before ReportStatus to
	// populate the token split.
	_ = ext.AdvertiseSystemPrompt(ctx, state)
	_ = ext.TurnPreamble(ctx, state)
	raw := ext.ReportStatus(ctx, state)
	if raw == nil {
		t.Fatalf("ReportStatus = nil after catalogue render")
	}
	var first map[string]any
	if err := json.Unmarshal(raw, &first); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if v, ok := first["loaded_skill_tokens"]; ok {
		t.Errorf("loaded_skill_tokens = %v before any skill loaded, want absent", v)
	}
	catOnly, ok := first["available_skill_tokens"].(float64)
	if !ok || catOnly <= 0 {
		t.Fatalf("available_skill_tokens missing/zero on catalogue-only render: %v", first)
	}

	// Load alpha and re-render. Loaded side now carries the
	// loaded-bundles meta + instructions + tag advice; catalogue
	// still advertises alpha (with `(loaded)` tag).
	if err := FromState(state).Load(ctx, "alpha"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	_ = ext.AdvertiseSystemPrompt(ctx, state)
	_ = ext.TurnPreamble(ctx, state)
	raw = ext.ReportStatus(ctx, state)
	var second map[string]any
	if err := json.Unmarshal(raw, &second); err != nil {
		t.Fatalf("post-load payload unmarshal: %v", err)
	}
	loadedAfter, ok := second["loaded_skill_tokens"].(float64)
	if !ok || loadedAfter <= 0 {
		t.Fatalf("loaded_skill_tokens missing/zero after Load: %v", second)
	}
	catAfter, ok := second["available_skill_tokens"].(float64)
	if !ok || catAfter <= 0 {
		t.Fatalf("available_skill_tokens missing/zero after Load: %v", second)
	}
	if total, ok := second["advertise_tokens"].(float64); !ok || total < loadedAfter+catAfter {
		t.Errorf("advertise_tokens (%v) should be ≥ loaded+catalogue (%v + %v)",
			total, loadedAfter, catAfter)
	}
}

// Loading alpha surfaces its body in the system prompt
// (AdvertiseSystemPrompt) and tags it `(loaded)` in the turn_preamble
// catalogue. Phase 6.x split the two halves: the loaded body stays in
// the stable/cacheable system prompt; the volatile catalogue (with
// the `(loaded)` annotation) rides the turn_preamble.
func TestLoadedBodyInSystemPrompt_CatalogueInTurnPreamble(t *testing.T) {
	ctx := context.Background()
	store := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{"alpha": []byte(inlineAlphaManifest)}})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "a1")
	state := fixture.NewTestSessionState("ses-adv2").WithDepth(2)
	if err := ext.InitState(ctx, state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	if err := FromState(state).Load(ctx, "alpha"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	sys := ext.AdvertiseSystemPrompt(ctx, state)
	if !strings.Contains(sys, "body") {
		t.Errorf("loaded skill body missing from system prompt: %s", sys)
	}
	if strings.Contains(sys, "## Available skills") {
		t.Errorf("catalogue leaked into system prompt: %s", sys)
	}
	pre := ext.TurnPreamble(ctx, state)
	if !strings.Contains(pre, "(loaded)") {
		t.Errorf("loaded tag missing from turn_preamble: %s", pre)
	}
	if !strings.Contains(pre, "## Available skills") {
		t.Errorf("catalogue heading missing from turn_preamble: %s", pre)
	}
}

// TestFilterTools_TriStateUnion verifies the four cases of
// phase-4.2 §3.1.2 union resolution. Union semantics fall out
// implicitly at the bindings layer (extension.go::collectBindings):
// absent allowed-tools contribute nothing, so admission =
// "any explicit grant from any loaded skill admits".
func TestFilterTools_TriStateUnion(t *testing.T) {
	const explicitFooManifest = `---
name: explicit-foo
description: explicit grant.
allowed-tools:
  - bash-mcp:bash.run
---
`
	const explicitEmptyManifest = `---
name: explicit-empty
description: explicit empty list — reference-only.
allowed-tools: []
---
`
	const absentManifest = `---
name: absent-grants
description: agentskills.io "do not restrict" — inherits union.
---
`
	const explicitBarManifest = `---
name: explicit-bar
description: another explicit grant.
allowed-tools:
  - hugr-main:discovery-list
---
`

	in := []tool.Tool{
		{Name: "bash-mcp:bash.run", Provider: "bash-mcp"},
		{Name: "hugr-main:discovery-list", Provider: "hugr-main"},
		{Name: "python:run_code", Provider: "python"},
	}

	cases := []struct {
		name      string
		toLoad    []string
		manifests map[string][]byte
		want      []string
	}{
		{
			name:   "absent_alone_grants_nothing",
			toLoad: []string{"absent-grants"},
			manifests: map[string][]byte{
				"absent-grants": []byte(absentManifest),
			},
			want: nil,
		},
		{
			name:   "absent_plus_explicit_inherits_via_union",
			toLoad: []string{"absent-grants", "explicit-foo"},
			manifests: map[string][]byte{
				"absent-grants": []byte(absentManifest),
				"explicit-foo":  []byte(explicitFooManifest),
			},
			want: []string{"bash-mcp:bash.run"},
		},
		{
			name:   "explicit_empty_alone_grants_nothing",
			toLoad: []string{"explicit-empty"},
			manifests: map[string][]byte{
				"explicit-empty": []byte(explicitEmptyManifest),
			},
			want: nil,
		},
		{
			name:   "explicit_empty_plus_explicit_only_explicit_grants",
			toLoad: []string{"explicit-empty", "explicit-foo"},
			manifests: map[string][]byte{
				"explicit-empty": []byte(explicitEmptyManifest),
				"explicit-foo":   []byte(explicitFooManifest),
			},
			want: []string{"bash-mcp:bash.run"},
		},
		{
			name:   "two_explicit_no_cross_contribution",
			toLoad: []string{"explicit-foo", "explicit-bar"},
			manifests: map[string][]byte{
				"explicit-foo": []byte(explicitFooManifest),
				"explicit-bar": []byte(explicitBarManifest),
			},
			want: []string{"bash-mcp:bash.run", "hugr-main:discovery-list"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := skillpkg.NewSkillStore(skillpkg.Options{Inline: tc.manifests})
			mgr := skillpkg.NewSkillManager(store, nil)
			ext := NewExtension(mgr, nil, "agent-tri-"+tc.name)
			state := fixture.NewTestSessionState("ses-tri-" + tc.name).WithDepth(2)
			if err := ext.InitState(ctx, state); err != nil {
				t.Fatalf("InitState: %v", err)
			}
			for _, n := range tc.toLoad {
				if err := FromState(state).Load(ctx, n); err != nil {
					t.Fatalf("Load %q: %v", n, err)
				}
			}
			out := ext.FilterTools(ctx, state, in)
			got := map[string]bool{}
			for _, t := range out {
				got[t.Name] = true
			}
			for _, w := range tc.want {
				if !got[w] {
					t.Errorf("missing %q in filtered set; got %v", w, got)
				}
			}
			for n := range got {
				found := false
				for _, w := range tc.want {
					if n == w {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("unexpected %q in filtered set", n)
				}
			}
		})
	}
}

// TestAdvertise_LoadedSkillsMeta_SkippedWhenNoneLoaded verifies the
// new "## Loaded skill bundles" block is omitted when no skill is
// loaded — the catalogue alone surfaces.
func TestAdvertise_LoadedSkillsMeta_SkippedWhenNoneLoaded(t *testing.T) {
	ctx := context.Background()
	store := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{"alpha": []byte(inlineAlphaManifest)}})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "a-meta-empty")
	state := fixture.NewTestSessionState("ses-meta-empty").WithDepth(2)
	if err := ext.InitState(ctx, state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	out := ext.TurnPreamble(ctx, state)
	if strings.Contains(out, "## Loaded skill bundles") {
		t.Errorf("loaded-skills meta block surfaced with no skills loaded:\n%s", out)
	}
}

// TestAdvertise_LoadedSkillsMeta_InlineSkill_HeaderOnly verifies an
// inline (no on-disk Root, no FS) loaded skill emits the header line
// + description but no directory: / scripts: / etc.
func TestAdvertise_LoadedSkillsMeta_InlineSkill_HeaderOnly(t *testing.T) {
	ctx := context.Background()
	store := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{"alpha": []byte(inlineAlphaManifest)}})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "a-meta-inline")
	state := fixture.NewTestSessionState("ses-meta-inline").WithDepth(2)
	if err := ext.InitState(ctx, state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	if err := FromState(state).Load(ctx, "alpha"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	out := ext.TurnPreamble(ctx, state)
	if !strings.Contains(out, "## Loaded skill bundles") {
		t.Errorf("meta block missing for loaded inline skill:\n%s", out)
	}
	if !strings.Contains(out, "Loaded skill: `alpha`") {
		t.Errorf("alpha header missing:\n%s", out)
	}
	// Inline skill has no on-disk directory or files.
	if strings.Contains(out, "  directory:") {
		t.Errorf("inline skill should not have directory line:\n%s", out)
	}
	if strings.Contains(out, "  scripts:") || strings.Contains(out, "  references:") || strings.Contains(out, "  assets:") {
		t.Errorf("inline skill should not have category listings:\n%s", out)
	}
}

// TestAdvertise_LoadedSkillsMeta_OnDiskFullBundle verifies an
// on-disk skill with all three categories emits directory + each
// category section with sorted file paths.
func TestAdvertise_LoadedSkillsMeta_OnDiskFullBundle(t *testing.T) {
	ctx := context.Background()
	root := writeBundledSkill(t, "delta", `---
name: delta
description: full bundle on disk.
license: MIT
---
delta body
`, map[string][]byte{
		"scripts/run.py":         []byte("print('run')"),
		"scripts/helper.py":      []byte("print('help')"),
		"references/howto.md":    []byte("how"),
		"references/deep/why.md": []byte("why"),
		"assets/template.html":   []byte("<html/>"),
	})

	store := skillpkg.NewSkillStore(skillpkg.Options{LocalRoot: root})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "a-meta-full")
	state := fixture.NewTestSessionState("ses-meta-full").WithDepth(2)
	if err := ext.InitState(ctx, state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	if err := FromState(state).Load(ctx, "delta"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	out := ext.TurnPreamble(ctx, state)

	checks := []string{
		"Loaded skill: `delta`",
		"  directory: ",
		"  description: full bundle on disk.",
		"  scripts:",
		"    - scripts/helper.py",
		"    - scripts/run.py",
		"  references:",
		"    - references/deep/why.md",
		"    - references/howto.md",
		"  assets:",
		"    - assets/template.html",
	}
	for _, c := range checks {
		if !strings.Contains(out, c) {
			t.Errorf("output missing %q\nfull output:\n%s", c, out)
		}
	}
	// scripts must appear before references (sorted-by-category-name
	// in writeBundleCategory's invocation order: scripts, references,
	// assets).
	idxS := strings.Index(out, "  scripts:")
	idxR := strings.Index(out, "  references:")
	idxA := strings.Index(out, "  assets:")
	if !(idxS < idxR && idxR < idxA) {
		t.Errorf("category order wrong: scripts@%d references@%d assets@%d", idxS, idxR, idxA)
	}
}

// TestAdvertise_LoadedSkillsMeta_PartialCategories verifies only
// non-empty categories surface — a skill with only scripts/ has no
// references: or assets: lines.
func TestAdvertise_LoadedSkillsMeta_PartialCategories(t *testing.T) {
	ctx := context.Background()
	root := writeBundledSkill(t, "scripts-only", `---
name: scripts-only
description: just scripts.
license: MIT
---
`, map[string][]byte{
		"scripts/just.py": []byte("print('only')"),
	})
	store := skillpkg.NewSkillStore(skillpkg.Options{LocalRoot: root})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "a-meta-partial")
	state := fixture.NewTestSessionState("ses-meta-partial").WithDepth(2)
	if err := ext.InitState(ctx, state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	if err := FromState(state).Load(ctx, "scripts-only"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	out := ext.TurnPreamble(ctx, state)

	if !strings.Contains(out, "  scripts:") {
		t.Errorf("expected scripts: section:\n%s", out)
	}
	if strings.Contains(out, "  references:") {
		t.Errorf("references: leaked when none present:\n%s", out)
	}
	if strings.Contains(out, "  assets:") {
		t.Errorf("assets: leaked when none present:\n%s", out)
	}
}

// TestAdvertise_LoadedSkillsMeta_StableOrder — multiple loaded
// skills appear in name-sorted order so prefix-cache is not
// disturbed.
func TestAdvertise_LoadedSkillsMeta_StableOrder(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for _, name := range []string{"zeta", "beta", "iota"} {
		writeBundledSkillInto(t, root, name, `---
name: `+name+`
description: order check.
license: MIT
---
`, nil)
	}
	store := skillpkg.NewSkillStore(skillpkg.Options{LocalRoot: root})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "a-meta-order")
	state := fixture.NewTestSessionState("ses-meta-order").WithDepth(2)
	if err := ext.InitState(ctx, state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	for _, n := range []string{"zeta", "beta", "iota"} {
		if err := FromState(state).Load(ctx, n); err != nil {
			t.Fatalf("Load %s: %v", n, err)
		}
	}
	out := ext.TurnPreamble(ctx, state)
	// Sorted by name → beta, iota, zeta.
	idxBeta := strings.Index(out, "Loaded skill: `beta`")
	idxIota := strings.Index(out, "Loaded skill: `iota`")
	idxZeta := strings.Index(out, "Loaded skill: `zeta`")
	if !(idxBeta < idxIota && idxIota < idxZeta) {
		t.Errorf("loaded-skills order wrong: beta@%d iota@%d zeta@%d", idxBeta, idxIota, idxZeta)
	}
}

// writeBundledSkill creates a fresh temp dir, writes a skill named
// `name` under it with SKILL.md + the supplied bundle files, and
// returns the temp dir (suitable for SkillStore Options.LocalRoot
// or SystemRoot).
func writeBundledSkill(t *testing.T, name, manifest string, files map[string][]byte) string {
	t.Helper()
	root := t.TempDir()
	writeBundledSkillInto(t, root, name, manifest, files)
	return root
}

// TestApplyOnSubagentSpawn_AutoloadSkills covers the role-declared
// autoload path: when a worker is spawned with role R declaring
// `autoload_skills: [target]`, the SubagentSpawnApplier loads
// `target` on the child's SessionSkill BEFORE the worker's first
// turn — no in-band `skill:load(...)` ritual needed.
func TestApplyOnSubagentSpawn_AutoloadSkills(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	// Dispatching skill with one role that pre-declares autoload.
	writeBundledSkillInto(t, root, "host", `---
name: host
description: dispatching skill with one autoloading role.
license: MIT
metadata:
  hugen:
    tier_compatibility: [mission]
    sub_agents:
      - name: worker-a
        description: worker that needs target pre-loaded.
        autoload_skills: [target]
---
`, nil)

	// The autoload target — worker-loadable.
	writeBundledSkillInto(t, root, "target", `---
name: target
description: provider the autoloading role needs.
license: MIT
metadata:
  hugen:
    tier_compatibility: [worker]
---
`, nil)

	store := skillpkg.NewSkillStore(skillpkg.Options{LocalRoot: root})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "agent-autoload")

	// Child session as the worker. Depth 2 → worker tier.
	child := fixture.NewTestSessionState("ses-child").WithDepth(2)
	if err := ext.InitState(ctx, child); err != nil {
		t.Fatalf("InitState child: %v", err)
	}

	// Sanity: target is NOT loaded yet (tier autoload only pulls
	// skills opting into `autoload_for`).
	if names := FromState(child).LoadedNames(ctx); contains(names, "target") {
		t.Fatalf("precondition: target should not be loaded yet, got %v", names)
	}

	// Invoke the applier as the runtime would post-Spawn.
	if err := ext.ApplyOnSubagentSpawn(ctx, child, "host", "worker-a"); err != nil {
		t.Fatalf("ApplyOnSubagentSpawn: %v", err)
	}

	// target should now be in the child's loaded set.
	if names := FromState(child).LoadedNames(ctx); !contains(names, "target") {
		t.Errorf("ApplyOnSubagentSpawn did not load 'target'; loaded=%v", names)
	}
}

// TestApplyOnSubagentSpawn_NoOpWhenRoleEmpty — empty role / skill /
// missing autoload list short-circuits without touching the child.
func TestApplyOnSubagentSpawn_NoOpWhenRoleEmpty(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeBundledSkillInto(t, root, "host", `---
name: host
description: dispatching skill, role declares no autoload.
license: MIT
metadata:
  hugen:
    tier_compatibility: [mission]
    sub_agents:
      - name: worker-a
        description: vanilla worker.
---
`, nil)
	store := skillpkg.NewSkillStore(skillpkg.Options{LocalRoot: root})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "agent-noop")
	child := fixture.NewTestSessionState("ses-noop").WithDepth(2)
	if err := ext.InitState(ctx, child); err != nil {
		t.Fatalf("InitState child: %v", err)
	}
	loadedBefore := append([]string(nil), FromState(child).LoadedNames(ctx)...)
	if err := ext.ApplyOnSubagentSpawn(ctx, child, "host", "worker-a"); err != nil {
		t.Errorf("unexpected error on no-op applier: %v", err)
	}
	loadedAfter := FromState(child).LoadedNames(ctx)
	if len(loadedAfter) != len(loadedBefore) {
		t.Errorf("loaded set mutated by no-op applier: before=%v after=%v", loadedBefore, loadedAfter)
	}
	// Unknown role / skill must also be silent no-ops.
	if err := ext.ApplyOnSubagentSpawn(ctx, child, "host", "missing-role"); err != nil {
		t.Errorf("unknown role should no-op, got: %v", err)
	}
	if err := ext.ApplyOnSubagentSpawn(ctx, child, "missing-skill", "worker-a"); err != nil {
		t.Errorf("unknown skill should no-op, got: %v", err)
	}
}

// writeBundledSkillInto writes a skill named `name` under root with
// SKILL.md + bundle files. Used when several skills share one root
// (TestAdvertise_LoadedSkillsMeta_StableOrder).
func writeBundledSkillInto(t *testing.T, root, name, manifest string, files map[string][]byte) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	for rel, data := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
}

// TestMergeRoleTools_GrantsRoleSpecificSurface covers the Phase
// I.18 wiring: a SubAgentRole.Tools entry on the dispatching skill
// admits the named tools onto a child session whose (Skill, Role)
// pair matches the role declaration — WITHOUT the child having to
// load the dispatching skill into its own SessionSkill bindings.
//
// The test uses a non-loaded host skill (only registered in the
// store, not Load()ed on the child) and verifies that
// FilterTools admits `host-provider:special` purely on the basis
// of the role's Tools grant.
func TestMergeRoleTools_GrantsRoleSpecificSurface(t *testing.T) {
	ctx := context.Background()
	const inlineHost = `---
name: host
description: host skill declaring a role with a tool grant.
license: MIT
metadata:
  hugen:
    tier_compatibility: [mission]
    sub_agents:
      - name: planner
        description: role with role-specific tool grant.
        tools:
          - provider: host-provider
            tools:
              - special
              - approval-*
            requires_approval:
              - special
---
host body
`
	store := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{
		"host": []byte(inlineHost),
	}})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "a-rolectx")

	in := []tool.Tool{
		{Name: "host-provider:special", Provider: "host-provider"},
		{Name: "host-provider:approval-approve", Provider: "host-provider"},
		{Name: "host-provider:other", Provider: "host-provider"},
		{Name: "unrelated:unrelated", Provider: "unrelated"},
	}

	// Worker session matching the (skill=host, role=planner) pair.
	// Skill NOT loaded into child's bindings — the role-side grant
	// is the only admission path.
	roleChild := fixture.NewTestSessionState("ses-role-match").
		WithDepth(2).
		WithSkill("host").
		WithRole("planner")
	if err := ext.InitState(ctx, roleChild); err != nil {
		t.Fatalf("InitState role-match: %v", err)
	}
	got := map[string]bool{}
	for _, tt := range ext.FilterTools(ctx, roleChild, in) {
		got[tt.Name] = true
	}
	if !got["host-provider:special"] {
		t.Errorf("special should be admitted via role-tool grant; got %v", got)
	}
	if !got["host-provider:approval-approve"] {
		t.Errorf("approval-* wildcard should match approval-approve; got %v", got)
	}
	if got["host-provider:other"] {
		t.Errorf("other should NOT be admitted; got %v", got)
	}

	// requires_approval on the role grant should propagate.
	var specialTool *tool.Tool
	for _, tt := range ext.FilterTools(ctx, roleChild, in) {
		if tt.Name == "host-provider:special" {
			t2 := tt
			specialTool = &t2
		}
	}
	if specialTool == nil {
		t.Fatal("host-provider:special missing from filtered set")
	}
	if !specialTool.RequiresApproval {
		t.Errorf("RequiresApproval = false for special; want true (role declared it under requires_approval)")
	}

	// Worker without matching role pair → no admission.
	roleless := fixture.NewTestSessionState("ses-role-empty").WithDepth(2)
	if err := ext.InitState(ctx, roleless); err != nil {
		t.Fatalf("InitState roleless: %v", err)
	}
	emptyOut := map[string]bool{}
	for _, tt := range ext.FilterTools(ctx, roleless, in) {
		emptyOut[tt.Name] = true
	}
	if emptyOut["host-provider:special"] {
		t.Errorf("special leaked through without a matching role pair; got %v", emptyOut)
	}

	// Worker with matching skill but unknown role → no admission.
	wrongRole := fixture.NewTestSessionState("ses-role-mis").
		WithDepth(2).
		WithSkill("host").
		WithRole("not-a-real-role")
	if err := ext.InitState(ctx, wrongRole); err != nil {
		t.Fatalf("InitState wrong-role: %v", err)
	}
	wrongOut := map[string]bool{}
	for _, tt := range ext.FilterTools(ctx, wrongRole, in) {
		wrongOut[tt.Name] = true
	}
	if wrongOut["host-provider:special"] {
		t.Errorf("special leaked for unknown role; got %v", wrongOut)
	}
}

// Empty store → AdvertiseSystemPrompt returns "" (skipped section).
func TestAdvertiseSystemPrompt_EmptyStore(t *testing.T) {
	store := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{}})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "a1")
	state := fixture.NewTestSessionState("ses-empty").WithDepth(2)
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	out := ext.AdvertiseSystemPrompt(context.Background(), state)
	if out != "" {
		t.Errorf("expected empty advertisement, got %q", out)
	}
}

// TestOnToolError_MatchesLoadedHint verifies the ModelInTurnAdvisor
// error-shaped match through the single OnToolResult variation: a
// LOADED skill's hint fires when its tool glob + regex match a result
// carrying the error text — whether that text rode in as a runtime
// error message (Code+ResultText) or inside a "successful" provider
// body (ResultText only). An installed-but-not-loaded skill stays
// silent. The inline manifest declares the hint as `on_tool_error`,
// exercising the parse-time alias to on_tool_result.
func TestOnToolError_MatchesLoadedHint(t *testing.T) {
	ctx := context.Background()
	store := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{
		"hinted": []byte(inlineHintManifest),
	}})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "a1")
	state := fixture.NewTestSessionState("ses-hint").WithDepth(2)
	if err := ext.InitState(ctx, state); err != nil {
		t.Fatalf("InitState: %v", err)
	}

	// Runtime-error shape: the message travels in ResultText, Code set.
	ev := extension.ToolResultEvent{
		Tool:       "hugr-main:data-inline_graphql_result",
		Code:       "validation",
		ResultText: `Cannot query field "core_modules_aggregation" on type "Query"`,
	}

	// Installed but not loaded → no contribution.
	if got := ext.OnToolResult(ctx, state, ev); got != "" {
		t.Errorf("unloaded skill hint fired: %q", got)
	}

	// Load it → hint fires.
	if err := FromState(state).Load(ctx, "hinted"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := ext.OnToolResult(ctx, state, ev)
	if !strings.Contains(got, "discovery-search_modules") {
		t.Errorf("loaded hint did not fire: %q", got)
	}

	// Non-matching error → no contribution even when loaded.
	noMatch := extension.ToolResultEvent{Tool: "hugr-main:data-inline_graphql_result", ResultText: "unknown filter operator"}
	if got := ext.OnToolResult(ctx, state, noMatch); got != "" {
		t.Errorf("non-matching error produced a hint: %q", got)
	}

	// Success-envelope failure (body only, no Code) matches the same hint.
	embedded := extension.ToolResultEvent{
		Tool:       "hugr-main:data-inline_graphql_result",
		ResultText: `{"is_error":true,"text":"Cannot query field \"x_aggregation\""}`,
	}
	if got := ext.OnToolResult(ctx, state, embedded); !strings.Contains(got, "discovery-search_modules") {
		t.Errorf("success-envelope failure hint did not fire: %q", got)
	}
}

// TestOnToolResult_MatchesLoadedHint verifies the success-path twin:
// a LOADED skill's on_tool_result hint fires on a truncated (but
// successful) result body, stays silent when unloaded, and does not
// fire on a non-matching result.
func TestOnToolResult_MatchesLoadedHint(t *testing.T) {
	ctx := context.Background()
	store := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{
		"hinted": []byte(inlineHintManifest),
	}})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "a1")
	state := fixture.NewTestSessionState("ses-hint-result").WithDepth(2)
	if err := ext.InitState(ctx, state); err != nil {
		t.Fatalf("InitState: %v", err)
	}

	ev := extension.ToolResultEvent{
		Tool:       "hugr-main:data-inline_graphql_result",
		ResultText: `{"data":{"rows":[1,2,3]},"is_truncated":true}`,
	}

	// Installed but not loaded → no contribution.
	if got := ext.OnToolResult(ctx, state, ev); got != "" {
		t.Errorf("unloaded skill result-hint fired: %q", got)
	}

	// Load it → hint fires on the truncated body.
	if err := FromState(state).Load(ctx, "hinted"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := ext.OnToolResult(ctx, state, ev); !strings.Contains(got, "hugr-query file output") {
		t.Errorf("loaded result-hint did not fire: %q", got)
	}

	// Non-truncated success → no contribution.
	noMatch := extension.ToolResultEvent{
		Tool:       "hugr-main:data-inline_graphql_result",
		ResultText: `{"data":{"rows":[1]},"is_truncated":false}`,
	}
	if got := ext.OnToolResult(ctx, state, noMatch); got != "" {
		t.Errorf("non-truncated result produced a hint: %q", got)
	}

	// A different tool → no contribution (tool glob).
	otherTool := extension.ToolResultEvent{Tool: "hugr-query:query", ResultText: `{"is_truncated":true}`}
	if got := ext.OnToolResult(ctx, state, otherTool); got != "" {
		t.Errorf("non-matching tool produced a hint: %q", got)
	}
}
