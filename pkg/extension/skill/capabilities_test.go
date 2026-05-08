package skill

import (
	"context"
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
