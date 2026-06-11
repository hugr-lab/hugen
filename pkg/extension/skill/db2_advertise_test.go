package skill

import (
	"context"
	"fmt"
	"io/fs"
	"testing"

	"github.com/hugr-lab/hugen/pkg/extension/recap"
	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	"github.com/hugr-lab/hugen/pkg/protocol"
	skillpkg "github.com/hugr-lab/hugen/pkg/skill"
)

// fakeRecallStore is a minimal SkillStore that ALSO implements the
// (unexported, in pkg/skill) skillRecaller surface via a matching method
// signature, so SkillManager.RecallRanked routes to it. Counts calls so a
// test can prove the per-turn recall cadence.
type fakeRecallStore struct {
	skills  []skillpkg.Skill
	dynamic []skillpkg.RecallCandidate
	pinned  []skillpkg.RecallCandidate
	err     error
	calls   int
}

func (f *fakeRecallStore) List(context.Context) ([]skillpkg.Skill, error) { return f.skills, nil }

func (f *fakeRecallStore) Get(_ context.Context, name string) (skillpkg.Skill, error) {
	for _, s := range f.skills {
		if s.Manifest.Name == name {
			return s, nil
		}
	}
	return skillpkg.Skill{}, skillpkg.ErrSkillNotFound
}

func (f *fakeRecallStore) Publish(context.Context, skillpkg.Manifest, fs.FS, skillpkg.PublishOptions) error {
	return nil
}

func (f *fakeRecallStore) RecallRanked(_ context.Context, _ string, _ int) (dynamic, pinned []skillpkg.RecallCandidate, err error) {
	f.calls++
	return f.dynamic, f.pinned, f.err
}

// twelveCandidates builds a dynamic candidate pool large enough that the
// top-N cut is exercised.
func twelveCandidates() []skillpkg.RecallCandidate {
	out := make([]skillpkg.RecallCandidate, 0, 12)
	for i := 0; i < 12; i++ {
		out = append(out, skillpkg.RecallCandidate{
			ID: fmt.Sprintf("skl-d%02d", i), Name: fmt.Sprintf("dyn-%02d", i), Shown: 10, Used: 5,
		})
	}
	return out
}

// seedRecap gives the root session a recap marker by feeding one user turn
// through the recap ext (Router nil → no fold, so CurrentRecap returns the
// raw-ring fallback — a non-empty anchor).
func seedRecap(t *testing.T, ctx context.Context, state *fixture.TestSessionState, text string) {
	t.Helper()
	rext := recap.NewExtension(recap.Deps{}, recap.Config{})
	if err := rext.InitState(ctx, state); err != nil {
		t.Fatalf("recap InitState: %v", err)
	}
	rext.OnFrameEmit(ctx, state, &protocol.UserMessage{Payload: protocol.UserMessagePayload{Text: text}})
	if _, ok := recap.CurrentRecap(state); !ok {
		t.Fatal("recap seed produced no anchor")
	}
}

// TestRankedAdvertise_TopNPlusPinned drives the full root path: a recap
// anchor + embedder yields a Thompson-sampled top-N dynamic ∪ all-pinned
// allow-map, cached across re-renders within the same turn.
func TestRankedAdvertise_TopNPlusPinned(t *testing.T) {
	store := &fakeRecallStore{
		dynamic: twelveCandidates(),
		pinned:  []skillpkg.RecallCandidate{{ID: "skl-p1", Name: "pinned-one", Shown: 3, Used: 1}},
	}
	ext := NewExtension(skillpkg.NewSkillManager(store, nil), nil, "a1")
	ctx := context.Background()
	state := fixture.NewTestSessionState("ses-root")
	if err := ext.InitState(ctx, state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	h := FromState(state)
	seedRecap(t, ctx, state, "analysing weekly sales deltas by region")

	allow, ok := ext.rankedAdvertise(ctx, state, h)
	if !ok {
		t.Fatal("ranked advertise should be ok for root + recap + embedder")
	}
	if len(allow) != recallTopN+1 {
		t.Errorf("allow size = %d, want %d (top-N %d + 1 pinned)", len(allow), recallTopN+1, recallTopN)
	}
	if _, pinnedShown := allow["pinned-one"]; !pinnedShown {
		t.Error("pinned skill must always be advertised, regardless of the draw")
	}
	for name := range allow {
		if _, ok := store.lookupName(name); !ok {
			t.Errorf("advertised name %q is not a recall candidate", name)
		}
	}

	// Second render in the SAME turn reuses the cached draw — no re-pull.
	if _, ok := ext.rankedAdvertise(ctx, state, h); !ok {
		t.Fatal("second render should still be ok")
	}
	if store.calls != 1 {
		t.Errorf("within-turn re-render must reuse the draw; calls = %d, want 1", store.calls)
	}
}

// TestRankedAdvertise_RepoolsPerTurn verifies the rotation cadence: the
// draw is rolled ONCE per turn and reused across the turn's model
// iterations (byte-stable catalogue), then a new user_message re-pools
// under the current topic — so the menu tracks pivots (re-pool, not freeze).
func TestRankedAdvertise_RepoolsPerTurn(t *testing.T) {
	store := &fakeRecallStore{dynamic: twelveCandidates()}
	ext := NewExtension(skillpkg.NewSkillManager(store, nil), nil, "a1")
	ctx := context.Background()
	state := fixture.NewTestSessionState("ses-root")
	if err := ext.InitState(ctx, state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	h := FromState(state)
	seedRecap(t, ctx, state, "topic one")

	a1, ok := ext.rankedAdvertise(ctx, state, h)
	if !ok {
		t.Fatal("first roll should be ok")
	}
	a2, _ := ext.rankedAdvertise(ctx, state, h)
	if !sameSet(a1, a2) {
		t.Errorf("draw changed within a turn: %v vs %v", keys(a1), keys(a2))
	}
	if store.calls != 1 {
		t.Errorf("within-turn renders must not re-pool; calls = %d, want 1", store.calls)
	}

	// A new user_message arms a re-roll → re-pool under the current topic.
	h.markAdvertiseRoll()
	if _, ok := h.cachedDraw(); ok {
		t.Error("markAdvertiseRoll should force a re-roll (cachedDraw false)")
	}
	if _, ok := ext.rankedAdvertise(ctx, state, h); !ok {
		t.Fatal("re-roll should be ok")
	}
	if store.calls != 2 {
		t.Errorf("a new turn must re-pool the catalogue; calls = %d, want 2", store.calls)
	}
}

// TestRankedAdvertise_SubagentUsesFirstMessage covers the universal anchor:
// a subagent ranks on its recap marker too (recap now runs for every
// session, distilling the worker's delegated task into the anchor).
func TestRankedAdvertise_SubagentUsesRecap(t *testing.T) {
	store := &fakeRecallStore{dynamic: twelveCandidates()}
	ext := NewExtension(skillpkg.NewSkillManager(store, nil), nil, "a1")
	ctx := context.Background()
	worker := fixture.NewTestSessionState("ses-w").WithDepth(1) // worker (depth>0)
	if err := ext.InitState(ctx, worker); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	h := FromState(worker)

	// No anchor yet (no recap marker) → fall back.
	if _, ok := ext.rankedAdvertise(ctx, worker, h); ok {
		t.Fatal("no anchor yet → must fall back")
	}

	// The worker's delegated task seeds its recap (runs for subagents now).
	seedRecap(t, ctx, worker, "build the html report for op2023 with plotly charts")

	allow, ok := ext.rankedAdvertise(ctx, worker, h)
	if !ok {
		t.Fatal("subagent with a recap marker should rank on it")
	}
	if len(allow) != recallTopN {
		t.Errorf("allow size = %d, want %d", len(allow), recallTopN)
	}
}

// TestRankedAdvertise_Fallbacks covers the (nil,false) → full-catalogue
// fallbacks: no anchor at all, and no embedder.
func TestRankedAdvertise_Fallbacks(t *testing.T) {
	ctx := context.Background()

	t.Run("no anchor", func(t *testing.T) {
		store := &fakeRecallStore{dynamic: []skillpkg.RecallCandidate{{Name: "d"}}}
		ext := NewExtension(skillpkg.NewSkillManager(store, nil), nil, "a1")
		state := fixture.NewTestSessionState("ses-root")
		_ = ext.InitState(ctx, state)
		h := FromState(state)
		// No recap, no captured brief → no anchor → fall back.
		if _, ok := ext.rankedAdvertise(ctx, state, h); ok {
			t.Error("no anchor → fall back to full catalogue")
		}
		if store.calls != 0 {
			t.Errorf("no anchor must not query recall; calls = %d", store.calls)
		}
	})

	t.Run("no embedder", func(t *testing.T) {
		store := &fakeRecallStore{err: skillpkg.ErrNoEmbedder}
		ext := NewExtension(skillpkg.NewSkillManager(store, nil), nil, "a1")
		state := fixture.NewTestSessionState("ses-root")
		_ = ext.InitState(ctx, state)
		h := FromState(state)
		seedRecap(t, ctx, state, "some topic")
		if _, ok := ext.rankedAdvertise(ctx, state, h); ok {
			t.Error("ErrNoEmbedder → fall back to full catalogue")
		}
		// A hard error is NOT cached, so a retry re-queries.
		if _, ok := ext.rankedAdvertise(ctx, state, h); ok {
			t.Error("still fall back on the retry")
		}
		if store.calls != 2 {
			t.Errorf("hard error must not be cached; calls = %d, want 2", store.calls)
		}
	})
}

// sameSet reports set equality of two name-key maps.
func sameSet(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// lookupName reports whether name is one of the store's candidates.
func (f *fakeRecallStore) lookupName(name string) (skillpkg.RecallCandidate, bool) {
	for _, c := range append(append([]skillpkg.RecallCandidate(nil), f.dynamic...), f.pinned...) {
		if c.Name == name {
			return c, true
		}
	}
	return skillpkg.RecallCandidate{}, false
}
