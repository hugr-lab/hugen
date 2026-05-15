package tui

import (
	"log/slog"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

func newHistoryTab(t *testing.T) (*tab, *[]string) {
	t.Helper()
	var saved []string
	tb := newTab("ses-hist", protocol.ParticipantInfo{ID: "u", Name: "u"},
		func(protocol.Frame) error { return nil }, slog.Default())
	tb.applyGeometry(80, 20, 100)
	tb.historySaver = func(_ string, h []string) { saved = append([]string(nil), h...) }
	return tb, &saved
}

func TestAppendHistory_PrependsAndDedupes(t *testing.T) {
	tb, saved := newHistoryTab(t)

	tb.appendHistory("hello")
	tb.appendHistory("how are you")
	tb.appendHistory("show sales")
	// Duplicate of most-recent should NOT grow the list (and
	// shouldn't reset historyIdx to something other than -1
	// which would surprise a follow-up Up).
	tb.appendHistory("show sales")

	want := []string{"show sales", "how are you", "hello"}
	if len(tb.history) != len(want) {
		t.Fatalf("history len = %d; want %d (%v)", len(tb.history), len(want), tb.history)
	}
	for i, v := range want {
		if tb.history[i] != v {
			t.Errorf("history[%d] = %q; want %q", i, tb.history[i], v)
		}
	}
	if got := *saved; len(got) == 0 {
		t.Errorf("historySaver never invoked")
	}
}

func TestAppendHistory_CollapsesOlderDuplicates(t *testing.T) {
	tb, _ := newHistoryTab(t)
	tb.appendHistory("hello")
	tb.appendHistory("how are you")
	// Resubmitting "hello" should move it to the front, not leave
	// two "hello" entries.
	tb.appendHistory("hello")
	if len(tb.history) != 2 {
		t.Fatalf("history len = %d; want 2 (got %v)", len(tb.history), tb.history)
	}
	if tb.history[0] != "hello" || tb.history[1] != "how are you" {
		t.Errorf("history order = %v; want [hello, how are you]", tb.history)
	}
}

func TestAppendHistory_CapsAtMaxPerTab(t *testing.T) {
	tb, _ := newHistoryTab(t)
	for i := 0; i < maxHistoryPerTab+10; i++ {
		tb.appendHistory(string(rune('a' + (i % 26))))
	}
	if len(tb.history) > maxHistoryPerTab {
		t.Errorf("history len = %d; want <= %d", len(tb.history), maxHistoryPerTab)
	}
}

func TestHistoryPrev_LoadsMostRecentAndAdvances(t *testing.T) {
	tb, _ := newHistoryTab(t)
	tb.appendHistory("first")
	tb.appendHistory("second")
	tb.appendHistory("third")

	if !tb.historyPrev() {
		t.Fatal("first Up should load most recent (third)")
	}
	if got := tb.textarea.Value(); got != "third" {
		t.Errorf("after first Up: textarea = %q; want third", got)
	}
	if !tb.historyPrev() {
		t.Fatal("second Up should step to second")
	}
	if got := tb.textarea.Value(); got != "second" {
		t.Errorf("after second Up: textarea = %q; want second", got)
	}
	if !tb.historyPrev() {
		t.Fatal("third Up should step to first")
	}
	if got := tb.textarea.Value(); got != "first" {
		t.Errorf("after third Up: textarea = %q; want first", got)
	}
	// At the oldest entry, further Up is a no-op.
	if tb.historyPrev() {
		t.Errorf("Up past oldest entry should be a no-op")
	}
}

func TestHistoryNext_WalksBackAndClearsAtZero(t *testing.T) {
	tb, _ := newHistoryTab(t)
	tb.appendHistory("first")
	tb.appendHistory("second")
	// Walk to oldest, then Down twice.
	tb.historyPrev()
	tb.historyPrev()
	if got := tb.textarea.Value(); got != "first" {
		t.Fatalf("walked to %q; want first", got)
	}
	if !tb.historyNext() {
		t.Fatal("Down from oldest should step to newer (second)")
	}
	if got := tb.textarea.Value(); got != "second" {
		t.Errorf("after Down: textarea = %q; want second", got)
	}
	if !tb.historyNext() {
		t.Fatal("Down from newest should clear textarea + reset idx")
	}
	if got := tb.textarea.Value(); got != "" {
		t.Errorf("after final Down: textarea = %q; want empty", got)
	}
	if tb.historyIdx != -1 {
		t.Errorf("historyIdx = %d; want -1 after exit", tb.historyIdx)
	}
}

func TestUpdateKey_UpRoutesToHistoryWhenEmpty(t *testing.T) {
	tb, _ := newHistoryTab(t)
	tb.appendHistory("hello")
	handled, _ := tb.updateKey(tea.KeyMsg{Type: tea.KeyUp})
	if !handled {
		t.Fatalf("Up on empty textarea must be handled")
	}
	if got := tb.textarea.Value(); got != "hello" {
		t.Errorf("Up on empty: textarea = %q; want hello", got)
	}
}

func TestUpdateKey_TypingResetsHistoryIdx(t *testing.T) {
	tb, _ := newHistoryTab(t)
	tb.appendHistory("hello")
	// Walk into history.
	tb.updateKey(tea.KeyMsg{Type: tea.KeyUp})
	if tb.historyIdx != 0 {
		t.Fatalf("after Up: historyIdx = %d; want 0", tb.historyIdx)
	}
	// Type a rune — historyIdx must reset.
	tb.updateKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if tb.historyIdx != -1 {
		t.Errorf("after typing: historyIdx = %d; want -1", tb.historyIdx)
	}
}

func TestEnterSubmit_AppendsHistory(t *testing.T) {
	tb, saved := newHistoryTab(t)
	tb.textarea.SetValue("hello world")
	tb.updateKey(tea.KeyMsg{Type: tea.KeyEnter})
	if len(tb.history) != 1 || tb.history[0] != "hello world" {
		t.Errorf("history after Enter = %v; want [hello world]", tb.history)
	}
	if len(*saved) != 1 || (*saved)[0] != "hello world" {
		t.Errorf("historySaver got = %v; want [hello world]", *saved)
	}
}
