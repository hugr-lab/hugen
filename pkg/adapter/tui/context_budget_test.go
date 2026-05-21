package tui

import (
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// TestRenderContextBudget_FullPane covers every dimension at
// once: history + tools + per-extension advertise + skills
// split + session usage. Every populated dimension shows up.
func TestRenderContextBudget_FullPane(t *testing.T) {
	b := &contextBudget{
		HistoryTokens: 12_000,
		ToolsTokens:   2_400,
		SessionUsage:  &protocol.TokenUsage{PromptTokens: 45_000, CompletionTokens: 8_000},
		Extensions: map[string]int{
			"compactor": 800,
			"notepad":   400,
			"plan":      200,
		},
		Skills: &skillsBudget{LoadedTokens: 1_500, AvailableTokens: 600},
	}
	out := renderContextBudget(b, 40)
	for _, want := range []string{
		"Context budget",
		"history",
		"12.0k",
		"tools",
		"2.4k",
		"compactor",
		"800",
		"notepad",
		"400",
		"plan",
		"200",
		"skill (loaded)",
		"1.5k",
		"skill (catalog)",
		"600",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("renderContextBudget missing %q:\n%s", want, out)
		}
	}
	// SessionUsage is rendered by renderSessionUsage now —
	// separate pane, separate test.
	if strings.Contains(out, "session") || strings.Contains(out, "45.0k") {
		t.Errorf("renderContextBudget should NOT include session usage; lives in its own pane:\n%s", out)
	}
}

// TestRenderSessionUsage covers the new ε.1 lifetime-usage
// pane. Cumulative numbers are surfaced separately so
// per-prompt and lifetime metrics don't read as comparable.
func TestRenderSessionUsage(t *testing.T) {
	u := &protocol.TokenUsage{PromptTokens: 643_200, CompletionTokens: 4_800}
	out := renderSessionUsage(u, 40)
	for _, want := range []string{
		"Usage (lifetime)",
		"prompt",
		"643.2k",
		"completion",
		"4.8k",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("renderSessionUsage missing %q:\n%s", want, out)
		}
	}
}

// TestRenderContextBudget_OmitsZeroDimensions — a fresh
// session with only tools tokens populated renders just the
// tools line.
func TestRenderContextBudget_OmitsZeroDimensions(t *testing.T) {
	b := &contextBudget{ToolsTokens: 500}
	out := renderContextBudget(b, 40)
	if !strings.Contains(out, "tools") {
		t.Errorf("tools line missing:\n%s", out)
	}
	for _, dontWant := range []string{
		"history", "compactor", "skill", "session",
	} {
		if strings.Contains(out, dontWant) {
			t.Errorf("renderContextBudget rendered empty dimension %q:\n%s", dontWant, out)
		}
	}
}

// TestFormatTokens covers the k / M suffix thresholds.
func TestFormatTokens(t *testing.T) {
	cases := map[int]string{
		0:         "0",
		500:       "500",
		999:       "999",
		1_000:     "1.0k",
		1_500:     "1.5k",
		45_000:    "45.0k",
		999_999:   "1000.0k",
		1_000_000: "1.0M",
		2_500_000: "2.5M",
	}
	for in, want := range cases {
		if got := formatTokens(in); got != want {
			t.Errorf("formatTokens(%d) = %q, want %q", in, got, want)
		}
	}
}
