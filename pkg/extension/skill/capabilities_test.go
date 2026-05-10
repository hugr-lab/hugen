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
	state := fixture.NewTestSessionState("ses-fil")
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

// nil SkillManager → no filter (every tool surfaces).
func TestFilterTools_NilManagerExposesEverything(t *testing.T) {
	ext := NewExtension(nil, nil, "a1")
	state := fixture.NewTestSessionState("ses-nil")
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
	state := fixture.NewTestSessionState("ses-empty")
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
	state := fixture.NewTestSessionState("ses-gen")
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

// Advertiser concatenates Bindings.Instructions with the catalogue
// section. With no skill loaded only the catalogue surfaces.
func TestAdvertiseSystemPrompt_CatalogueOnly(t *testing.T) {
	ctx := context.Background()
	store := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{"alpha": []byte(inlineAlphaManifest)}})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "a1")
	state := fixture.NewTestSessionState("ses-adv")
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
	state := fixture.NewTestSessionState("ses-adv2")
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
			state := fixture.NewTestSessionState("ses-tri-" + tc.name)
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
	state := fixture.NewTestSessionState("ses-meta-empty")
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
	state := fixture.NewTestSessionState("ses-meta-inline")
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
	state := fixture.NewTestSessionState("ses-meta-full")
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
	state := fixture.NewTestSessionState("ses-meta-partial")
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
	state := fixture.NewTestSessionState("ses-meta-order")
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
	state := fixture.NewTestSessionState("ses-empty")
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	out := ext.AdvertiseSystemPrompt(context.Background(), state)
	if out != "" {
		t.Errorf("expected empty advertisement, got %q", out)
	}
}
