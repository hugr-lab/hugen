package mission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/extension"
)

// TestCallGetResearch_SurfacesFileRefs verifies the research role's
// DECLARED artifact paths (handoff body.file_refs) reach a worker
// through the mission:get_research tool — the single channel the
// runtime uses to point workers at the research files now that the
// mission-files index is gone. Phase B31.
func TestCallGetResearch_SurfacesFileRefs(t *testing.T) {
	state := newFakeState("mis-gr")
	m := installMissionState(state)
	refs := []string{"research/data-model.md", "research/queries.md"}
	m.SetResearchOutput("scope decided", nil, nil, refs)

	// Persisted + lock-safe copy preserves order.
	if got := m.ResearchFileRefs(); len(got) != 2 || got[0] != refs[0] || got[1] != refs[1] {
		t.Fatalf("ResearchFileRefs() = %v, want %v", got, refs)
	}

	e := &Extension{}
	ctx := extension.WithSessionState(context.Background(), state)
	raw, err := e.callGetResearch(ctx, nil)
	if err != nil {
		t.Fatalf("callGetResearch: %v", err)
	}
	var resp getResearchResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !resp.Available {
		t.Fatalf("expected available=true, got %+v", resp)
	}
	if len(resp.FileRefs) != 2 || resp.FileRefs[0] != refs[0] || resp.FileRefs[1] != refs[1] {
		t.Errorf("response file_refs = %v, want %v", resp.FileRefs, refs)
	}
}

// TestCallGetResearch_NoFileRefsOmitted verifies file_refs is omitted
// (not a null/empty key) when the research role declared none.
func TestCallGetResearch_NoFileRefsOmitted(t *testing.T) {
	state := newFakeState("mis-gr2")
	m := installMissionState(state)
	m.SetResearchOutput("scope decided", nil, nil, nil)

	e := &Extension{}
	ctx := extension.WithSessionState(context.Background(), state)
	raw, err := e.callGetResearch(ctx, nil)
	if err != nil {
		t.Fatalf("callGetResearch: %v", err)
	}
	if got := string(raw); strings.Contains(got, "file_refs") {
		t.Errorf("file_refs should be omitted when none declared; got %s", got)
	}
}
