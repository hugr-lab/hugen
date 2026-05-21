package extension

// EstimateTokens is the cheap per-string heuristic the
// context-budget observability surface uses to size each
// extension's Advertise contribution. char/4 is the long-
// standing English rule of thumb; for other scripts it
// under-estimates, which is fine — the resulting number is a
// UI indicator (rendered in the TUI / webui context budget
// pane), not a hard budget cap.
//
// Phase 5.2 (context-budget observability, β).
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	// Round up so even short messages contribute at least 1.
	return (len(s) + 3) / 4
}
