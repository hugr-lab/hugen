package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestFormatThousands(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{42, "42"},
		{999, "999"},
		{1000, "1,000"},
		{1247, "1,247"},
		{1234567, "1,234,567"},
	}
	for _, c := range cases {
		if got := formatThousands(c.in); got != c.want {
			t.Errorf("formatThousands(%d) = %q; want %q", c.in, got, c.want)
		}
	}
}

func TestModel_StatsResultMsg_UpdatesActiveTabFooter(t *testing.T) {
	m, _ := newTestModel(t)
	id := m.tabs[0].sessionID
	m2, _ := m.Update(statsResultMsg{sessionID: id, events: 42})
	m = m2.(model)
	if m.tabs[0].eventsCount != 42 {
		t.Errorf("eventsCount = %d; want 42", m.tabs[0].eventsCount)
	}
	view := m.View()
	if !strings.Contains(view, "42 events") {
		t.Errorf("footer missing '42 events' in:\n%s", view)
	}
}

func TestModel_TickStatsMsg_SchedulesSampleAndReArm(t *testing.T) {
	m, _ := newTestModel(t)
	var sampled []string
	m.sampleStats = func(id string) tea.Cmd {
		sampled = append(sampled, id)
		return func() tea.Msg { return statsResultMsg{sessionID: id, events: 1} }
	}
	m2, cmd := m.Update(tickStatsMsg{})
	m = m2.(model)
	if cmd == nil {
		t.Fatal("tickStatsMsg must produce a tea.Cmd (sample + re-arm)")
	}
	if len(sampled) != 1 || sampled[0] != m.tabs[0].sessionID {
		t.Errorf("expected one sample for the active tab; got %v", sampled)
	}
}

func TestModel_StatsResultMsg_UnknownTabIsBenign(t *testing.T) {
	m, _ := newTestModel(t)
	m2, _ := m.Update(statsResultMsg{sessionID: "ses-unknown", events: 99})
	m = m2.(model)
	if m.tabs[0].eventsCount != -1 {
		t.Errorf("eventsCount should stay at -1 for unrelated stats; got %d", m.tabs[0].eventsCount)
	}
}

func TestRenderFooter_HidesEventsBeforeFirstSample(t *testing.T) {
	m, _ := newTestModel(t)
	// Initial state: eventsCount == -1 (no sample yet).
	if m.tabs[0].eventsCount != -1 {
		t.Fatalf("initial eventsCount = %d; want -1", m.tabs[0].eventsCount)
	}
	view := m.View()
	if strings.Contains(view, "events") {
		t.Errorf("footer should NOT render 'events' before first sample:\n%s", view)
	}
}
