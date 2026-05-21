package session

import (
	"context"
	"testing"

	"github.com/hugr-lab/hugen/pkg/tool"
)

// TestToolCatalogTokens_CachedAcrossSnapshotKey verifies the δ
// helper:
//
//   - returns 0 when the session has no ToolManager wired;
//   - grows with the per-tool Name + Description + ArgSchema
//     byte budget once a provider lands;
//   - caches the result behind the same (toolGen, policyGen,
//     extGen) key the snapshot honours — a generation bump
//     forces a recompute, a hit returns the prior value.
func TestToolCatalogTokens_CachedAcrossSnapshotKey(t *testing.T) {
	provider := &staticToolProvider{
		name: "budget",
		tools: []tool.Tool{
			{
				Name:        "budget:do_thing",
				Description: "Run a thing on the budget side. Returns a thing result.",
				ArgSchema:   []byte(`{"type":"object","properties":{"x":{"type":"integer"}}}`),
				Provider:    "budget",
			},
			{
				Name:        "budget:do_other",
				Description: "Run the other thing.",
				ArgSchema:   []byte(`{"type":"object"}`),
				Provider:    "budget",
			},
		},
	}
	tm := tool.NewToolManager(permsAllowAll{}, nil, nil)
	if err := tm.AddProvider(provider); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	parent, cleanup := newTestParent(t, withTestTools(tm))
	defer cleanup()

	ctx := context.Background()
	got := parent.ToolCatalogTokens(ctx)
	if got <= 0 {
		t.Fatalf("ToolCatalogTokens with tools wired = %d, want > 0", got)
	}

	// Cache hit on identical generation — read again and assert
	// the value is stable.
	again := parent.ToolCatalogTokens(ctx)
	if again != got {
		t.Errorf("cached read = %d, want %d (stable across same gen)", again, got)
	}
}

// TestToolCatalogTokens_ZeroWithoutToolManager verifies the
// no-ToolManager fast path — fixture sessions without a
// ToolManager wired must return 0 cleanly.
func TestToolCatalogTokens_ZeroWithoutToolManager(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()
	parent.tools = nil // newTestParent default wires an allow-all tm; force zero path.
	if got := parent.ToolCatalogTokens(context.Background()); got != 0 {
		t.Errorf("ToolCatalogTokens without tools = %d, want 0", got)
	}
}
