package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// renderTabBar returns the top-of-screen tab bar. Each tab gets a
// "[N glyph] short-id" cell; the active tab is bold/inverse, dirty
// tabs (activity since last focus) carry a `*` glyph + accent
// colour. Truncates from the right when total width would overflow.
// Slice 4 — phase 5.1c §8.
func renderTabBar(tabs []*tab, active int, width int) string {
	if width <= 0 || len(tabs) == 0 {
		return ""
	}
	var cells []string
	for i, t := range tabs {
		cells = append(cells, formatTabCell(t, i, i == active))
	}
	bar := strings.Join(cells, " ")
	if lipgloss.Width(bar) > width {
		bar = lipgloss.NewStyle().MaxWidth(width).Render(bar)
	}
	pad := width - lipgloss.Width(bar)
	if pad > 0 {
		bar += strings.Repeat(" ", pad)
	}
	return tabBarBgStyle.Render(bar)
}

func formatTabCell(t *tab, idx int, active bool) string {
	glyph := " "
	if t.dirty && !active {
		glyph = "*"
	}
	label := fmt.Sprintf(" %d%s %s ", idx+1, glyph, shortID(t.sessionID))
	switch {
	case active:
		return tabActiveStyle.Render(label)
	case t.dirty:
		return tabDirtyStyle.Render(label)
	default:
		return tabIdleStyle.Render(label)
	}
}

var (
	tabBarBgStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("236"))
	tabActiveStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("15")).
			Background(lipgloss.Color("24"))
	tabDirtyStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("11")).
			Background(lipgloss.Color("236"))
	tabIdleStyle = lipgloss.NewStyle().
			Faint(true).
			Background(lipgloss.Color("236"))
)
