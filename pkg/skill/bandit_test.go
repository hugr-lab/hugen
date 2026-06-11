package skill

import (
	"math/rand/v2"
	"testing"
)

// TestThompsonRank_FavorsProvenArm — a proven-good arm (90/100 loaded)
// should lead a proven-bad arm (1/100) in the vast majority of fresh
// draws: its Beta posterior sits high and tight, the bad one's near 0.
func TestThompsonRank_FavorsProvenArm(t *testing.T) {
	good := RecallCandidate{ID: "good", Shown: 100, Used: 90}
	bad := RecallCandidate{ID: "bad", Shown: 100, Used: 1}
	src := rand.NewPCG(1, 2)
	const N = 500
	goodFirst := 0
	for i := 0; i < N; i++ {
		cands := []RecallCandidate{bad, good} // deliberately bad-first
		ThompsonRank(cands, src)
		if cands[0].ID == "good" {
			goodFirst++
		}
	}
	if goodFirst < N*8/10 {
		t.Errorf("proven-good arm should lead most draws; got %d/%d", goodFirst, N)
	}
}

// TestThompsonRank_ColdArmExplores — a cold arm (0/0 = Beta(1,1) uniform)
// sometimes outdraws a proven-good arm: exploration is intrinsic, with no
// reserved explore slot — but it must not dominate.
func TestThompsonRank_ColdArmExplores(t *testing.T) {
	good := RecallCandidate{ID: "good", Shown: 50, Used: 45}
	cold := RecallCandidate{ID: "cold", Shown: 0, Used: 0}
	src := rand.NewPCG(7, 9)
	const N = 500
	coldFirst := 0
	for i := 0; i < N; i++ {
		cands := []RecallCandidate{good, cold}
		ThompsonRank(cands, src)
		if cands[0].ID == "cold" {
			coldFirst++
		}
	}
	if coldFirst == 0 {
		t.Error("cold arm never explored — Beta(1,1) should win sometimes")
	}
	if coldFirst > N/2 {
		t.Errorf("cold arm dominated (%d/%d) — should mostly defer to the proven arm", coldFirst, N)
	}
}

// TestThompsonRank_Edges — nil / single / used>shown clamp must not panic.
func TestThompsonRank_Edges(t *testing.T) {
	src := rand.NewPCG(1, 1)
	ThompsonRank(nil, src)
	ThompsonRank([]RecallCandidate{{ID: "x"}}, src)
	// used > shown (stale pair) clamps so Beta β stays ≥ 1.
	cands := []RecallCandidate{{ID: "y", Shown: 2, Used: 5}}
	ThompsonRank(cands, src)
	if cands[0].ID != "y" {
		t.Fatal("single candidate should survive")
	}
	// Negative counts (corrupt row) clamp to 0 — gonum's Beta would
	// return NaN on a non-positive parameter and poison the sort.
	neg := []RecallCandidate{{ID: "n", Shown: -5, Used: -2}, {ID: "p", Shown: 4, Used: 4}}
	ThompsonRank(neg, src)
	if len(neg) != 2 {
		t.Fatal("negative-count candidates should survive the rank")
	}
}
