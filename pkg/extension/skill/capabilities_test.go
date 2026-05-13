package skill

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	skillpkg "github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

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

// TestAdvertiseSystemPrompt_AvailableMissions_RootOnly verifies the
// "## Available missions" block appears in root-tier sessions only
// — mission/worker tier never see the dispatch catalogue because
// they cannot call session:spawn_mission. Phase 4.2.2 §6.
func TestAdvertiseSystemPrompt_AvailableMissions_RootOnly(t *testing.T) {
	ctx := context.Background()
	missionSkill := `---
name: analyst
description: data analysis skill.
metadata:
  hugen:
    tier_compatibility: [mission]
    mission:
      enabled: true
      summary: Data analysis, queries, reports.
---
body
`
	store := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{
		"analyst": []byte(missionSkill),
	}})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "a1")

	// root tier: block must render.
	rootState := fixture.NewTestSessionState("ses-root").WithDepth(0)
	if err := ext.InitState(ctx, rootState); err != nil {
		t.Fatalf("InitState root: %v", err)
	}
	rootOut := ext.AdvertiseSystemPrompt(ctx, rootState)
	if !strings.Contains(rootOut, "## Available missions") {
		t.Errorf("root prompt missing Available missions heading: %s", rootOut)
	}
	if !strings.Contains(rootOut, "`analyst`") {
		t.Errorf("root prompt missing analyst bullet: %s", rootOut)
	}
	if !strings.Contains(rootOut, "Data analysis") {
		t.Errorf("root prompt missing analyst summary: %s", rootOut)
	}

	// mission tier: block must NOT appear (mission cannot spawn
	// another mission; the catalogue is dead weight there).
	missState := fixture.NewTestSessionState("ses-mission").WithDepth(1)
	if err := ext.InitState(ctx, missState); err != nil {
		t.Fatalf("InitState mission: %v", err)
	}
	missOut := ext.AdvertiseSystemPrompt(ctx, missState)
	if strings.Contains(missOut, "## Available missions") {
		t.Errorf("mission prompt leaked Available missions: %s", missOut)
	}

	// worker tier: also no.
	wkState := fixture.NewTestSessionState("ses-worker").WithDepth(2)
	if err := ext.InitState(ctx, wkState); err != nil {
		t.Fatalf("InitState worker: %v", err)
	}
	wkOut := ext.AdvertiseSystemPrompt(ctx, wkState)
	if strings.Contains(wkOut, "## Available missions") {
		t.Errorf("worker prompt leaked Available missions: %s", wkOut)
	}
}

// TestAdvertiseSystemPrompt_NotepadTagsBlockA verifies the phase
// 4.2.3 Block A section ("## Notepad — recommended tags") appears
// when a mission-enabled skill with on_start.notepad.tags is
// loaded into the session.
func TestAdvertiseSystemPrompt_NotepadTagsBlockA(t *testing.T) {
	ctx := context.Background()
	missionWithTags := `---
name: analyst
description: data analysis skill.
metadata:
  hugen:
    tier_compatibility: [mission]
    mission:
      enabled: true
      summary: Data analysis, queries, reports.
      on_start:
        notepad:
          tags:
            - name: schema-finding
              hint: Discovered table structures or field semantics.
            - name: data-quality-issue
              hint: Anomalies, nulls, suspicious cardinalities.
            - name: deferred-question
---
body
`
	store := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{
		"analyst": []byte(missionWithTags),
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

	out := ext.AdvertiseSystemPrompt(ctx, missState)
	for _, want := range []string{
		"## Notepad — recommended tags for this mission",
		"`schema-finding`",
		"Discovered table structures",
		"`data-quality-issue`",
		"`deferred-question`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Block A missing %q; got:\n%s", want, out)
		}
	}
}

// TestAdvertiseSystemPrompt_NotepadTagsEmptyWhenNoMissionSkill —
// Block A omits entirely when no loaded skill declares notepad
// tags (e.g. workers, or missions whose dispatcher doesn't use
// the field).
func TestAdvertiseSystemPrompt_NotepadTagsEmptyWhenNoMissionSkill(t *testing.T) {
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
	out := ext.AdvertiseSystemPrompt(ctx, missState)
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

// Advertiser concatenates Bindings.Instructions with the catalogue
// section. With no skill loaded only the catalogue surfaces.
func TestAdvertiseSystemPrompt_CatalogueOnly(t *testing.T) {
	ctx := context.Background()
	store := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{"alpha": []byte(inlineAlphaManifest)}})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "a1")
	state := fixture.NewTestSessionState("ses-adv").WithDepth(2)
	if err := ext.InitState(ctx, state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	out := ext.AdvertiseSystemPrompt(ctx, state)
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

// Loading alpha tags it `(loaded)` and surfaces its body before
// the catalogue heading.
func TestAdvertiseSystemPrompt_LoadedTaggedAndInstructionsPrepended(t *testing.T) {
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
	out := ext.AdvertiseSystemPrompt(ctx, state)
	if !strings.Contains(out, "(loaded)") {
		t.Errorf("loaded tag missing: %s", out)
	}
	idxBody := strings.Index(out, "body")
	idxHeading := strings.Index(out, "## Available skills")
	if idxBody < 0 || idxHeading < 0 || idxBody >= idxHeading {
		t.Errorf("instructions block must precede catalogue: body@%d heading@%d", idxBody, idxHeading)
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
	out := ext.AdvertiseSystemPrompt(ctx, state)
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
	out := ext.AdvertiseSystemPrompt(ctx, state)
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
	out := ext.AdvertiseSystemPrompt(ctx, state)

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
	out := ext.AdvertiseSystemPrompt(ctx, state)

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
	out := ext.AdvertiseSystemPrompt(ctx, state)
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
