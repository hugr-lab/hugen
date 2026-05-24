package compactor

import (
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/model"
)

// TestPositionBasedCutoff exercises the worker-mode fallback the
// compactor uses when boundary math is unavailable but the token
// budget has tripped. Walks history newest→oldest accumulating
// estimated message tokens, returns the seq just before the entry
// where accumulated weight crosses MaxTokens/3.
//
// estimateTokens is roughly len(content)/4 with a +1 floor, so we
// construct contents whose token weight is predictable:
//   - 400 chars  ≈ 100 tokens
//   - 4000 chars ≈ 1000 tokens
func TestPositionBasedCutoff(t *testing.T) {
	mkEntry := func(seq int64, bytes int) HistoryEntry {
		return HistoryEntry{
			Seq:       seq,
			Timestamp: time.Unix(seq, 0),
			Message: model.Message{
				Role:    model.RoleAssistant,
				Content: strRepeat("a", bytes),
			},
		}
	}

	cases := []struct {
		name      string
		entries   []HistoryEntry
		maxTokens int
		want      int64
	}{
		{
			name:      "MaxTokens=0 disables fallback",
			entries:   []HistoryEntry{mkEntry(1, 4000), mkEntry(2, 4000)},
			maxTokens: 0,
			want:      0,
		},
		{
			name:      "empty history returns 0",
			entries:   nil,
			maxTokens: 30_000,
			want:      0,
		},
		{
			name:      "single-entry history returns 0",
			entries:   []HistoryEntry{mkEntry(5, 4000)},
			maxTokens: 30_000,
			want:      0,
		},
		{
			name: "whole history under preserved budget returns 0",
			// Total ~250 tokens; preserved budget = 30000/3 = 10000.
			entries: []HistoryEntry{
				mkEntry(1, 200), mkEntry(2, 200), mkEntry(3, 200), mkEntry(4, 400),
			},
			maxTokens: 30_000,
			want:      0,
		},
		{
			name: "accumulated weight crosses budget mid-walk",
			// preserved budget = 30000/3 = 10000 tokens.
			// Entries (newest last): 100 tok, 100 tok, 100 tok, 100 tok, 12000 tok.
			// Walking back: 12000 (seq=5) → accumulates to 12000 ≥ 10000.
			// Cutoff = seq(5) - 1 = 4 → everything ≤ 4 goes into the
			// compaction range; seq 5 onward stays verbatim.
			entries: []HistoryEntry{
				mkEntry(1, 400), mkEntry(2, 400), mkEntry(3, 400), mkEntry(4, 400),
				mkEntry(5, 48_000),
			},
			maxTokens: 30_000,
			want:      4,
		},
		{
			name: "single large recent entry crosses budget alone",
			// One huge entry at the tail, several small ones before.
			// preserved budget = 30000/3 = 10000.
			// Newest entry (seq=10) is 48000 chars ≈ 12000 tokens.
			// Cutoff = seq(10) - 1 = 9.
			entries: []HistoryEntry{
				mkEntry(7, 200), mkEntry(8, 200), mkEntry(9, 200),
				mkEntry(10, 48_000),
			},
			maxTokens: 30_000,
			want:      9,
		},
		{
			name: "first entry on backward walk crosses budget; cutoff would be 0 → return 0",
			// Only one entry but maxTokens triggers; the walk hits
			// entries[0] (seq=1), cutoff = 1 - 1 = 0 → guarded to 0.
			entries: []HistoryEntry{
				mkEntry(1, 48_000), mkEntry(2, 200),
			},
			maxTokens: 30_000,
			want:      0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &CompactorState{}
			s.resetHistory(tc.entries)
			cfg := Config{MaxTokens: tc.maxTokens}
			got := positionBasedCutoff(s, cfg)
			if got != tc.want {
				t.Errorf("positionBasedCutoff = %d, want %d", got, tc.want)
			}
		})
	}
}

// strRepeat avoids importing strings in this small test file when
// only the trivial repeat use case is needed.
func strRepeat(s string, n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, n*len(s))
	bp := 0
	for i := 0; i < n; i++ {
		bp += copy(b[bp:], s)
	}
	return string(b)
}
