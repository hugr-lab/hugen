package skill

import (
	"context"
	"testing"

	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	"github.com/hugr-lab/hugen/pkg/protocol"
	skillpkg "github.com/hugr-lab/hugen/pkg/skill"
)

// TestOnFrameEmit_ShownPendingPerTurn verifies the db-2 per-turn `shown`
// dedup: a user message arms the flag; takeShownPending consumes it
// exactly once (so the catalogue's per-iteration re-renders log one
// impression per turn); non-user frames never arm it.
func TestOnFrameEmit_ShownPendingPerTurn(t *testing.T) {
	ext := NewExtension(skillpkg.NewSkillManager(skillpkg.NewSkillStore(skillpkg.Options{}), nil), nil, "a1")
	ctx := context.Background()
	state := fixture.NewTestSessionState("ses-root")
	if err := ext.InitState(ctx, state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	h := FromState(state)
	if h == nil {
		t.Fatal("no SessionSkill handle")
	}

	if h.takeShownPending() {
		t.Error("flag should start cleared")
	}

	// A user message arms the flag.
	ext.OnFrameEmit(ctx, state, &protocol.UserMessage{Payload: protocol.UserMessagePayload{Text: "hi"}})
	if !h.takeShownPending() {
		t.Error("user message should arm shownPending")
	}
	// Consumed — a second take in the same turn is false (one impression).
	if h.takeShownPending() {
		t.Error("shownPending must be cleared after the first take")
	}

	// A non-user frame must NOT arm it.
	ext.OnFrameEmit(ctx, state, &protocol.AgentMessage{Payload: protocol.AgentMessagePayload{Text: "x", Consolidated: true, Final: true}})
	if h.takeShownPending() {
		t.Error("agent message must not arm shownPending")
	}
}

// TestIdsForNames_ResolvesViaShownCatalog verifies the db-2 in-memory
// id resolution: the `used` path resolves loaded names through the
// session's last-shown catalogue (no DB), so only skills actually shown
// (and indexed) resolve — autoloaded / never-shown names contribute
// nothing.
func TestIdsForNames_ResolvesViaShownCatalog(t *testing.T) {
	ext := NewExtension(skillpkg.NewSkillManager(skillpkg.NewSkillStore(skillpkg.Options{}), nil), nil, "a1")
	ctx := context.Background()
	state := fixture.NewTestSessionState("ses-root")
	if err := ext.InitState(ctx, state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	h := FromState(state)

	// No catalogue rendered yet → nothing resolves (e.g. autoload at
	// session start, before any render).
	if ids := h.idsForNames([]string{"hugr-data"}); len(ids) != 0 {
		t.Errorf("no catalogue → no ids, got %v", ids)
	}

	h.setShownCatalog(map[string]string{"hugr-data": "skl-de45", "analyst": "skl-1449"})
	got := h.idsForNames([]string{"hugr-data", "_root", "analyst", "unknown"})
	want := []string{"skl-de45", "skl-1449"} // shown+indexed only, in input order
	if len(got) != len(want) {
		t.Fatalf("idsForNames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("idsForNames[%d] = %q, want %q (full %v)", i, got[i], want[i], got)
		}
	}
}

// TestLogSkillEvents_NoDynamicBackendNoOp verifies the manager's
// LogSkillEvents is a safe no-op when the store has no dynamic backend
// (the fixture / inline store) — usage logging never fails the turn.
func TestLogSkillEvents_NoDynamicBackendNoOp(t *testing.T) {
	mgr := skillpkg.NewSkillManager(skillpkg.NewSkillStore(skillpkg.Options{}), nil)
	if err := mgr.LogSkillEvents(context.Background(), []string{"skl-1"}, skillpkg.SkillLogShown, "ses-x"); err != nil {
		t.Errorf("LogSkillEvents should no-op without a dynamic backend, got %v", err)
	}
}
