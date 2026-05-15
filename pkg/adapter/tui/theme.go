// Package tui — theme module. Slice 6 (phase 5.1c §10) covers two
// built-in palettes (dark + light) with $COLORFGBG auto-detection
// and a per-user override via ~/.hugen/tui.yaml. Operator-level
// override via config.yaml is deferred to a follow-up.
//
// The theme is a flat struct of [lipgloss.Color]s rather than a
// full style registry so we can keep the lipgloss style bodies
// (Bold / Faint / Italic / Border …) in their existing call sites.
// applyTheme rebuilds every package-level style var when a fresh
// theme is selected; no caller needs to know the theme is dynamic.
package tui

import (
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Theme is the immutable palette consumed by [applyTheme]. Field
// names mirror the semantic role rather than the historical
// lipgloss color codes — that keeps light / dark / future themes
// readable.
type Theme struct {
	Name string

	// Chat pane
	UserFG       lipgloss.Color
	ReasoningFG  lipgloss.Color
	SystemFG     lipgloss.Color
	ErrorFG      lipgloss.Color
	FaintFG      lipgloss.Color // status footer left + assistant Markdown caption tone

	// Sidebar
	SidebarBorderFG  lipgloss.Color
	SidebarHeading   lipgloss.Color
	SidebarActiveFG  lipgloss.Color
	SidebarPendingFG lipgloss.Color

	// Tab bar
	TabBarBG     lipgloss.Color
	TabActiveFG  lipgloss.Color
	TabActiveBG  lipgloss.Color
	TabDirtyFG   lipgloss.Color

	// HITL modal
	InquiryBorderFG lipgloss.Color
	InquiryTitleFG  lipgloss.Color
	InquiryHintFG   lipgloss.Color
}

// darkTheme is the original 5.1c slice 1 palette — designed for
// terminals with a dark background. Stays the default when
// $COLORFGBG is unset or unparseable.
var darkTheme = Theme{
	Name:             "dark",
	UserFG:           lipgloss.Color("12"),  // bright blue
	ReasoningFG:      "",                    // empty = inherit faint+italic
	SystemFG:         lipgloss.Color("8"),   // dim grey
	ErrorFG:          lipgloss.Color("9"),   // red
	FaintFG:          lipgloss.Color("8"),
	SidebarBorderFG:  lipgloss.Color("8"),
	SidebarHeading:   lipgloss.Color("12"),
	SidebarActiveFG:  lipgloss.Color("10"),  // green
	SidebarPendingFG: lipgloss.Color("11"),  // amber
	TabBarBG:         lipgloss.Color("236"), // dark grey
	TabActiveFG:      lipgloss.Color("15"),  // white
	TabActiveBG:      lipgloss.Color("24"),  // deep teal
	TabDirtyFG:       lipgloss.Color("11"),
	InquiryBorderFG:  lipgloss.Color("11"),
	InquiryTitleFG:   lipgloss.Color("11"),
	InquiryHintFG:    lipgloss.Color("12"),
}

// lightTheme remaps the saturated 16-color codes to higher-
// contrast equivalents that survive a light terminal background.
// Backgrounds prefer 254/253 (pale grey) instead of 236 so the
// tab bar still feels recessed.
var lightTheme = Theme{
	Name:             "light",
	UserFG:           lipgloss.Color("4"),   // standard blue
	ReasoningFG:      "",
	SystemFG:         lipgloss.Color("240"), // mid-grey
	ErrorFG:          lipgloss.Color("1"),   // dark red
	FaintFG:          lipgloss.Color("240"),
	SidebarBorderFG:  lipgloss.Color("240"),
	SidebarHeading:   lipgloss.Color("4"),
	SidebarActiveFG:  lipgloss.Color("2"),   // dark green
	SidebarPendingFG: lipgloss.Color("3"),   // dark yellow
	TabBarBG:         lipgloss.Color("254"), // pale grey
	TabActiveFG:      lipgloss.Color("0"),   // black
	TabActiveBG:      lipgloss.Color("117"), // light blue
	TabDirtyFG:       lipgloss.Color("3"),
	InquiryBorderFG:  lipgloss.Color("3"),
	InquiryTitleFG:   lipgloss.Color("3"),
	InquiryHintFG:    lipgloss.Color("4"),
}

// activeTheme is the package-level pointer used by every render
// path. Replaced wholesale by [applyTheme] on adapter startup.
var activeTheme = darkTheme

func init() {
	// Seed every package-level style var so tests that exercise
	// render paths without going through Adapter.Run still get a
	// fully-styled output. Adapter.Run re-applies the resolved
	// theme on startup; this just ensures a sane baseline.
	applyTheme(darkTheme)
}

// resolveTheme picks the effective Theme given a user override
// (from ~/.hugen/tui.yaml) and the auto-detect path. Precedence:
//
//  1. Explicit user override ("dark" / "light" / "auto"). "auto"
//     falls through to step 2.
//  2. $COLORFGBG env var. Values like "15;0" (white on black) or
//     "0;15" (black on white) — the FIRST value is the FG colour
//     code. FG > 7 ⇒ light text ⇒ dark theme; FG <= 7 ⇒ dark text
//     ⇒ light theme.
//  3. Default: dark.
func resolveTheme(override string) Theme {
	override = strings.ToLower(strings.TrimSpace(override))
	switch override {
	case "dark":
		return darkTheme
	case "light":
		return lightTheme
	}
	// auto-detect via COLORFGBG
	if v := strings.TrimSpace(os.Getenv("COLORFGBG")); v != "" {
		parts := strings.Split(v, ";")
		if len(parts) >= 2 {
			if fg, err := strconv.Atoi(strings.TrimSpace(parts[0])); err == nil {
				if fg <= 7 {
					return lightTheme
				}
				return darkTheme
			}
		}
	}
	return darkTheme
}

// applyTheme installs t as the active palette and rebuilds every
// package-level style var so the next View() picks up the new
// colours. Called once at adapter startup; tests call it directly
// to exercise alternate palettes.
//
// Returns the previously-active theme so callers can stash it
// (only tests use the return value today).
func applyTheme(t Theme) Theme {
	prev := activeTheme
	activeTheme = t

	// chat pane (frames.go)
	styleUser = lipgloss.NewStyle().Bold(true).Foreground(t.UserFG)
	styleReasoning = lipgloss.NewStyle().Faint(true).Italic(true)
	styleSystem = lipgloss.NewStyle().Faint(true).Foreground(t.SystemFG)

	// sidebar (sidebar.go + model.go border)
	sidebarBoxStyle = lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderLeft(true).
		BorderForeground(t.SidebarBorderFG).
		PaddingLeft(1)
	styleSidebarHeading = lipgloss.NewStyle().Bold(true).Foreground(t.SidebarHeading)
	styleSidebarFaint = lipgloss.NewStyle().Faint(true)
	styleSidebarActive = lipgloss.NewStyle().Foreground(t.SidebarActiveFG)
	stylePendingInquiry = lipgloss.NewStyle().Bold(true).Foreground(t.SidebarPendingFG)

	// inquiry modal
	inquiryBoxStyle = lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(t.InquiryBorderFG).
		Padding(0, 1)
	inquiryTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(t.InquiryTitleFG)
	inquiryFaintStyle = lipgloss.NewStyle().Faint(true)
	inquiryHintStyle = lipgloss.NewStyle().Foreground(t.InquiryHintFG)

	// tab bar
	tabBarBgStyle = lipgloss.NewStyle().Background(t.TabBarBG)
	tabActiveStyle = lipgloss.NewStyle().Bold(true).Foreground(t.TabActiveFG).Background(t.TabActiveBG)
	tabDirtyStyle = lipgloss.NewStyle().Foreground(t.TabDirtyFG).Background(t.TabBarBG)
	tabIdleStyle = lipgloss.NewStyle().Faint(true).Background(t.TabBarBG)
	return prev
}
