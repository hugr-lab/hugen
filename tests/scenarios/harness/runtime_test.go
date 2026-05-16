//go:build duckdb_arrow && scenario

package harness

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/oasdiff/yaml"
)

func TestMergeConfigs_DeepMerge(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.yaml")
	llm := filepath.Join(dir, "llm.yaml")
	topo := filepath.Join(dir, "topology.yaml")
	out := filepath.Join(dir, "out.yaml")

	// Base mirrors a stripped prod config.yaml shape.
	if err := os.WriteFile(base, []byte(`models:
  mode: local
  model: gemma4-26b
  routes:
    cheap:
      mode: local
      model: gemma-small
embedding:
  mode: local
  model: gemma-embedding
tool_providers:
  - name: bash-mcp
    type: mcp
  - name: hugr-main
    type: mcp
    transport: http
`), 0o600); err != nil {
		t.Fatal(err)
	}
	// LLM overlay flips just the default model; routes block must
	// survive untouched (it's nested under models).
	if err := os.WriteFile(llm, []byte(`models:
  model: claude-sonnet
`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Topology overlay replaces tool_providers (list, not map).
	if err := os.WriteFile(topo, []byte(`tool_providers:
  - name: bash-mcp
    type: mcp
`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := mergeConfigs(base, []string{llm, topo}, out); err != nil {
		t.Fatalf("mergeConfigs: %v", err)
	}

	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := yaml.Unmarshal(body, &got); err != nil {
		t.Fatalf("parse merged: %v", err)
	}

	models, ok := got["models"].(map[string]any)
	if !ok {
		t.Fatalf("models not a map: %T", got["models"])
	}
	if models["model"] != "claude-sonnet" {
		t.Errorf("models.model = %v, want claude-sonnet", models["model"])
	}
	if models["mode"] != "local" {
		t.Errorf("models.mode = %v, want local (preserved from base)", models["mode"])
	}
	routes, ok := models["routes"].(map[string]any)
	if !ok {
		t.Fatalf("models.routes not a map after merge: %T", models["routes"])
	}
	if cheap, ok := routes["cheap"].(map[string]any); !ok || cheap["model"] != "gemma-small" {
		t.Errorf("models.routes.cheap not preserved: %v", routes["cheap"])
	}

	providers, ok := got["tool_providers"].([]any)
	if !ok {
		t.Fatalf("tool_providers not a list: %T", got["tool_providers"])
	}
	if len(providers) != 1 {
		t.Errorf("tool_providers replaced wholesale; len = %d, want 1", len(providers))
	}

	embed, ok := got["embedding"].(map[string]any)
	if !ok || embed["model"] != "gemma-embedding" {
		t.Errorf("embedding block not preserved: %v", got["embedding"])
	}
}

// TestMergeConfigs_NoOverlaysReturnsBase verifies the variadic
// overlay list copes with an empty list (no LLM, no topology).
func TestMergeConfigs_NoOverlaysReturnsBase(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.yaml")
	out := filepath.Join(dir, "out.yaml")
	if err := os.WriteFile(base, []byte("models:\n  model: only-one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mergeConfigs(base, nil, out); err != nil {
		t.Fatalf("mergeConfigs: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	models, _ := got["models"].(map[string]any)
	if models["model"] != "only-one" {
		t.Errorf("merged model = %v, want only-one", models["model"])
	}
}

// TestMergeConfigs_ThreeOverlaysAppliedInOrder pins phase 5.2 ξ
// behaviour: a run's `overlays:` list layers on TOP of LLM +
// topology overlays. The last overlay wins on a contested key.
func TestMergeConfigs_ThreeOverlaysAppliedInOrder(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.yaml")
	llm := filepath.Join(dir, "llm.yaml")
	topo := filepath.Join(dir, "topo.yaml")
	extra := filepath.Join(dir, "extra.yaml")
	out := filepath.Join(dir, "out.yaml")

	if err := os.WriteFile(base, []byte("subagents:\n  parking:\n    parked_idle_timeout: 10m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// LLM overlay sets something unrelated.
	if err := os.WriteFile(llm, []byte("models:\n  model: claude-sonnet\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Topology overlay also sets something unrelated.
	if err := os.WriteFile(topo, []byte("tool_providers:\n  - name: bash-mcp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Extra overlay narrows parked_idle_timeout — must win over base.
	if err := os.WriteFile(extra, []byte("subagents:\n  parking:\n    parked_idle_timeout: 5s\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := mergeConfigs(base, []string{llm, topo, extra}, out); err != nil {
		t.Fatalf("mergeConfigs: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	subs, _ := got["subagents"].(map[string]any)
	parking, _ := subs["parking"].(map[string]any)
	if parking["parked_idle_timeout"] != "5s" {
		t.Errorf("parked_idle_timeout = %v, want 5s (overlays applied in order)",
			parking["parked_idle_timeout"])
	}
	// Verify the base/llm/topo keys survived too.
	models, _ := got["models"].(map[string]any)
	if models["model"] != "claude-sonnet" {
		t.Errorf("llm overlay lost: model = %v", models["model"])
	}
	providers, _ := got["tool_providers"].([]any)
	if len(providers) != 1 {
		t.Errorf("topology overlay lost: tool_providers len = %d", len(providers))
	}
}

func TestSortedKeys(t *testing.T) {
	got := SortedKeys(map[string]int{"b": 2, "a": 1, "c": 3})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
