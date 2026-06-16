package skill

import (
	"math/rand/v2"
	"sort"

	"gonum.org/v1/gonum/stat/distuv"
)

// RecallCandidate is one skill returned by the db-2 recall+counts query:
// the semantic candidate (id / name / description) plus its append-only
// usage tallies. The bandit ranks on these two numbers.
//
//   - Shown — how many times the skill was surfaced as a discovery
//     candidate (the impression / denominator).
//   - Used — how many times the model loaded it (the conversion / reward).
type RecallCandidate struct {
	ID          string
	Name        string
	Description string
	Shown       int
	Used        int

	// TaskEligible marks the candidate as a runnable task (a task-eligible
	// skill), so the advertise can split one recall into the `## Available
	// skills` and `## Available tasks` catalogues — each ranked + capped
	// against its own population (B47 step 5).
	TaskEligible bool
}

// ThompsonRank reorders cands in place by a FRESH Thompson sample: each
// candidate draws θ ~ Beta(Used+1, (Shown−Used)+1) and the slice sorts by
// θ descending. The Beta posterior makes exploration intrinsic — a cold
// arm (Beta(1,1) = uniform, wide) sometimes draws high and surfaces; a
// proven-bad arm (many shown, ~0 used → tight near 0) drops out — with no
// reserved explore slot or optimistic prior. Drawing fresh per advertise
// gives natural per-message rotation.
//
// src carries the randomness; the caller controls seeding (a fresh source
// per advertise for rotation, a fixed seed in tests for determinism).
// Counts are clamped to 0 ≤ Used ≤ Shown so the Beta parameters stay ≥ 1
// even if a stale or corrupt count pair slips through — gonum's Beta
// returns NaN on non-positive parameters, which would poison the sort.
func ThompsonRank(cands []RecallCandidate, src rand.Source) {
	type scored struct {
		c     RecallCandidate
		theta float64
	}
	arr := make([]scored, len(cands))
	for i, c := range cands {
		used := max(c.Used, 0)
		shown := max(c.Shown, 0)
		if used > shown {
			used = shown
		}
		beta := distuv.Beta{
			Alpha: float64(used + 1),
			Beta:  float64(shown-used) + 1,
			Src:   src,
		}
		arr[i] = scored{c: c, theta: beta.Rand()}
	}
	sort.SliceStable(arr, func(i, j int) bool { return arr[i].theta > arr[j].theta })
	for i := range arr {
		cands[i] = arr[i].c
	}
}
