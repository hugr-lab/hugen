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
// test can prove the per-topic recall cache.
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

// seedRecap populates the root session's recap tail with one user turn so
// recap.CurrentRecap(state) returns a non-empty effective topic (Topic
// stays "" — no fold without a Router).
func seedRecap(t *testing.T, ctx context.Context, state *fixture.TestSessionState, text string) {
	t.Helper()
	rext := recap.NewExtension(recap.Deps{}, recap.Config{})
	if err := rext.InitState(ctx, state); err != nil {
		t.Fatalf("recap InitState: %v", err)
	}
	u := &protocol.UserMessage{Payload: protocol.UserMessagePayload{Text: text}}
	u.SetSeq(1) // > watermark(0) so the tail accepts it
	rext.OnFrameEmit(ctx, state, u)
	if _, ok := recap.CurrentRecap(state); !ok {
		t.Fatal("recap seed produced no effective topic")
	}
}

// TestRecallShouldRefresh covers the cached-pool refresh decision: build
// once when cold, reuse on the same topic, and re-pull on a topic shift
// only when the change cleared the confidence gate — with the pre-fold
// (cachedKey=="") first-fold force.
func TestRecallShouldRefresh(t *testing.T) {
	cases := []struct {
		name       string
		valid      bool
		cachedKey  string
		topic      string
		confidence float64
		want       bool
	}{
		{"cold cache always builds", false, "", "", 0, true},
		{"same empty topic reuses", true, "", "", 0, false},
		{"same named topic reuses", true, "sales", "sales", 0.9, false},
		{"first fold forces refresh (key empty, low conf)", true, "", "sales", 0.0, true},
		{"topic shift below gate reuses", true, "sales", "weather", recallRefreshConfidence - 0.01, false},
		{"topic shift at gate refreshes", true, "sales", "weather", recallRefreshConfidence, true},
		{"topic shift above gate refreshes", true, "sales", "weather", 0.99, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := recallShouldRefresh(tc.valid, tc.cachedKey, tc.topic, tc.confidence)
			if got != tc.want {
				t.Errorf("recallShouldRefresh(%v,%q,%q,%v) = %v, want %v",
					tc.valid, tc.cachedKey, tc.topic, tc.confidence, got, tc.want)
			}
		})
	}
}

// TestRecallCache_LoadReturnsCopies verifies loadRecall hands back an
// independent slice — the caller (rankedAdvertise) sorts it in place with
// ThompsonRank, which must NOT reorder the cached pool.
func TestRecallCache_LoadReturnsCopies(t *testing.T) {
	h := &SessionSkill{}
	if _, _, ok := h.loadRecall(); ok {
		t.Error("empty cache should report not-ok")
	}
	h.storeRecall("topic", []skillpkg.RecallCandidate{{Name: "a"}, {Name: "b"}}, nil)
	got, _, ok := h.loadRecall()
	if !ok || len(got) != 2 {
		t.Fatalf("loadRecall = %v, %v", got, ok)
	}
	got[0], got[1] = got[1], got[0] // reorder the loaded copy in place
	again, _, _ := h.loadRecall()
	if again[0].Name != "a" {
		t.Errorf("cached pool order corrupted by caller mutation: got %q, want %q", again[0].Name, "a")
	}
	if key, valid := h.recallState(); !valid || key != "topic" {
		t.Errorf("recallState = %q,%v want topic,true", key, valid)
	}
}

// TestRankedAdvertise_TopNPlusPinned drives the full root path: a topic +
// embedder yields a Thompson-sampled top-N dynamic ∪ all-pinned allow-map,
// and the recall query is cached across re-renders of the same topic.
func TestRankedAdvertise_TopNPlusPinned(t *testing.T) {
	dyn := make([]skillpkg.RecallCandidate, 0, 12)
	for i := 0; i < 12; i++ {
		dyn = append(dyn, skillpkg.RecallCandidate{
			ID: fmt.Sprintf("skl-d%02d", i), Name: fmt.Sprintf("dyn-%02d", i), Shown: 10, Used: 5,
		})
	}
	store := &fakeRecallStore{
		dynamic: dyn,
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
	// Every survivor name must be a real candidate (a dyn-NN or the pin).
	for name := range allow {
		if _, ok := store.lookupName(name); !ok {
			t.Errorf("advertised name %q is not a recall candidate", name)
		}
	}

	// Second render on the SAME topic reuses the cached pool — no re-pull.
	if _, ok := ext.rankedAdvertise(ctx, state, h); !ok {
		t.Fatal("second render should still be ok")
	}
	if store.calls != 1 {
		t.Errorf("recall query should be cached on the same topic; calls = %d, want 1", store.calls)
	}
}

// TestRankedAdvertise_DrawStableWithinTurn verifies the rotation cadence:
// the Thompson selection is rolled ONCE per turn and reused across the
// turn's model iterations (byte-stable catalogue past the KV-cache
// boundary), then re-rolled only when a new user_message arms the latch —
// without re-running the (cached) recall query for the same topic.
func TestRankedAdvertise_DrawStableWithinTurn(t *testing.T) {
	dyn := make([]skillpkg.RecallCandidate, 0, 12)
	for i := 0; i < 12; i++ {
		dyn = append(dyn, skillpkg.RecallCandidate{
			ID: fmt.Sprintf("skl-d%02d", i), Name: fmt.Sprintf("dyn-%02d", i), Shown: 10, Used: 5,
		})
	}
	store := &fakeRecallStore{dynamic: dyn}
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
	// Re-render within the SAME turn → identical selection, no re-roll.
	a2, _ := ext.rankedAdvertise(ctx, state, h)
	if !sameSet(a1, a2) {
		t.Errorf("draw changed within a turn: %v vs %v", keys(a1), keys(a2))
	}

	// A new user_message arms a re-roll; recall is still cached (same
	// topic) so only Thompson re-runs — store.calls must stay 1.
	h.markAdvertiseRoll()
	if _, ok := h.cachedDraw(); ok {
		t.Error("markAdvertiseRoll should force a re-roll (cachedDraw false)")
	}
	if _, ok := ext.rankedAdvertise(ctx, state, h); !ok {
		t.Fatal("re-roll should be ok")
	}
	if store.calls != 1 {
		t.Errorf("same topic → recall cached across re-roll; calls = %d, want 1", store.calls)
	}
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

// TestRankedAdvertise_Fallbacks covers the (nil,false) → full-catalogue
// fallbacks: non-root tier, no recap yet, and no embedder.
func TestRankedAdvertise_Fallbacks(t *testing.T) {
	ctx := context.Background()

	t.Run("non-root tier", func(t *testing.T) {
		store := &fakeRecallStore{dynamic: []skillpkg.RecallCandidate{{Name: "d"}}}
		ext := NewExtension(skillpkg.NewSkillManager(store, nil), nil, "a1")
		state := fixture.NewTestSessionState("ses-w").WithDepth(1) // tier = worker
		_ = ext.InitState(ctx, state)
		h := FromState(state)
		if _, ok := ext.rankedAdvertise(ctx, state, h); ok {
			t.Error("non-root session must fall back, not rank")
		}
		if store.calls != 0 {
			t.Errorf("non-root must not query recall; calls = %d", store.calls)
		}
	})

	t.Run("no recap yet", func(t *testing.T) {
		store := &fakeRecallStore{dynamic: []skillpkg.RecallCandidate{{Name: "d"}}}
		ext := NewExtension(skillpkg.NewSkillManager(store, nil), nil, "a1")
		state := fixture.NewTestSessionState("ses-root")
		_ = ext.InitState(ctx, state)
		h := FromState(state)
		if _, ok := ext.rankedAdvertise(ctx, state, h); ok {
			t.Error("no recap → fall back to full catalogue")
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
		// A hard error is NOT cached, so a retry next turn re-queries.
		if _, ok := ext.rankedAdvertise(ctx, state, h); ok {
			t.Error("still fall back on the retry")
		}
		if store.calls != 2 {
			t.Errorf("hard error must not be cached; calls = %d, want 2", store.calls)
		}
	})
}
