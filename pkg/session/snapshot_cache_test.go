package session

import (
	"context"
	"testing"

	"github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// Wildcard grants in a skill manifest must expand at filter time
// against the live tool list. Phase 4.1a stage A step 8 moved
// this filtering out of pkg/tool.ToolManager into pkg/session;
// the test that previously lived in pkg/tool/manager_test.go
// retargets the new applySkillFilter helper.
func TestApplySkillFilter_AllowedToolsWildcardMatches(t *testing.T) {
	ctx := context.Background()

	manifest := []byte(`---
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
`)
	store := skill.NewSkillStore(skill.Options{Inline: map[string][]byte{"dataset": manifest}})
	skills := skill.NewSkillManager(store, nil)
	if err := skills.Load(ctx, "s1", "dataset"); err != nil {
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
	for _, t := range applySkillFilter(ctx, skills, "s1", in) {
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

// nil skills → no filter (every tool surfaces).
func TestApplySkillFilter_NilSkillsExposesEverything(t *testing.T) {
	in := []tool.Tool{
		{Name: "a:x", Provider: "a"},
		{Name: "b:y", Provider: "b"},
	}
	got := applySkillFilter(context.Background(), nil, "s", in)
	if len(got) != 2 {
		t.Errorf("filter with nil skills dropped tools: got %v", got)
	}
}

// Skills wired but no skill loaded → empty filtered set
// (distinguishes "no SkillManager" from "no skills loaded").
func TestApplySkillFilter_NoSkillLoadedYieldsEmpty(t *testing.T) {
	store := skill.NewSkillStore(skill.Options{Inline: map[string][]byte{}})
	skills := skill.NewSkillManager(store, nil)
	in := []tool.Tool{{Name: "a:x", Provider: "a"}}
	got := applySkillFilter(context.Background(), skills, "s", in)
	if len(got) != 0 {
		t.Errorf("expected empty catalogue, got %v", got)
	}
}

func TestAllowedSet_Match(t *testing.T) {
	a := &allowedSet{
		exact:    map[string]bool{"a:b": true},
		patterns: []string{"a:p"}, // matches a:p<anything>
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
	// nil set → unconditional match.
	var n *allowedSet
	if !n.match("anything") {
		t.Errorf("nil allowedSet should match unconditionally")
	}
}
