package tool

import (
	"context"
	"encoding/json"
	"testing"
)

// TestSnapshotSanitizesArgSchemaForLLM verifies the universal net in
// rebuildSnapshot: every tool's arg schema in the snapshot conforms to
// the conservative all-provider subset, even when a system provider
// hands back a non-conformant schema (here `additionalProperties`,
// which Gemini 400s for the entire tools payload). MCP tools are cleaned
// at their own boundary; this guards our in-repo + task: provider tools.
func TestSnapshotSanitizesArgSchemaForLLM(t *testing.T) {
	m := NewToolManager(&fakePerms{}, nil, nil)
	p := &fakeProvider{name: "sys", tools: []Tool{{
		Name:     "sys:run",
		Provider: "sys",
		ArgSchema: json.RawMessage(
			`{"type":"object","properties":{"inputs":{"type":"object","additionalProperties":true}},"required":["inputs"]}`),
	}}}
	if err := m.AddProvider(p); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}

	snap, err := m.Snapshot(context.Background(), "s1")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	var found bool
	for _, tl := range snap.Tools {
		if tl.Name != "sys:run" {
			continue
		}
		found = true
		if err := ValidateLLMSchema(tl.ArgSchema); err != nil {
			t.Errorf("snapshot arg schema not LLM-conformant: %v\nschema=%s", err, tl.ArgSchema)
		}
	}
	if !found {
		t.Fatal("sys:run missing from snapshot")
	}
}
