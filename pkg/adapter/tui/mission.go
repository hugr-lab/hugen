package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// missionRow is a snapshot of one in-flight mission as displayed in
// the `/mission` modal. Built from liveviewStatus.Children plus
// liveviewStatus.ChildMeta at modal-open time. Phase 5.1c.cancel-ux.
type missionRow struct {
	SessionID string
	Tier      string // "mission" / "worker" — read from depth
	Role      string // childMeta.Role
	Goal      string // childMeta.Task — truncated to fit
	StartedAt time.Time
	// Cancelling flips true between the operator's `c` and the
	// runtime's SubagentResult cascade; the row stays in the modal
	// during that gap rendered as "⊘ cancelling…".
	Cancelling bool
}

// missionModalState carries everything the modal needs to render +
// dispatch. Lives on tab; mirrors the inquiry-modal pattern.
type missionModalState struct {
	rows     []missionRow
	selected int
}

// snapshotMissions builds the row list from the current liveview
// projection. Only direct children of the tab's root are listed —
// workers spawned under a mission are reached via the mission's
// own cancel cascade. Returns an empty slice when the projection
// is missing or has no children.
func snapshotMissions(s *liveviewStatus) []missionRow {
	if s == nil || len(s.Children) == 0 {
		return nil
	}
	ids := make([]string, 0, len(s.Children))
	for id := range s.Children {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]missionRow, 0, len(ids))
	for _, id := range ids {
		c := s.Children[id]
		if c == nil {
			continue
		}
		meta := s.ChildMeta[id]
		out = append(out, missionRow{
			SessionID: id,
			Tier:      shortTierLabel(c.Depth),
			Role:      meta.Role,
			Goal:      strings.TrimSpace(meta.Task),
			StartedAt: meta.StartedAt,
		})
	}
	return out
}

// newMissionModalState builds the modal handle from the tab's current
// sidebar projection. Returns a state even when the list is empty —
// the operator should see "no missions running" rather than no
// feedback.
func newMissionModalState(s *liveviewStatus) *missionModalState {
	return &missionModalState{rows: snapshotMissions(s)}
}

// renderMissionModal returns the framed modal block. Width is the
// horizontal budget for the modal (same contract as
// renderInquiryModal).
func renderMissionModal(state *missionModalState, width int) string {
	if width < 40 {
		width = 40
	}
	contentW := width - 4
	if contentW < 20 {
		contentW = 20
	}

	var sb strings.Builder
	sb.WriteString(missionTitleStyle.Render("Cancel a mission"))
	sb.WriteString("\n\n")

	if len(state.rows) == 0 {
		sb.WriteString(missionFaintStyle.Render("No missions running."))
		sb.WriteString("\n\n")
		sb.WriteString(missionHintStyle.Render("[esc] dismiss"))
		return missionBoxStyle.Width(width - 2).Render(sb.String())
	}

	for i, r := range state.rows {
		leader := "  "
		if i == state.selected {
			leader = "▸ "
		}
		label := r.Tier
		if r.Role != "" {
			label = label + ":" + r.Role
		}
		goal := r.Goal
		if goal == "" {
			goal = "(no goal recorded)"
		}
		age := ""
		if !r.StartedAt.IsZero() {
			age = " · " + ageString(r.StartedAt)
		}
		body := fmt.Sprintf("%s · %q%s", label, goal, age)
		// "⊘ cancelling…" replaces the body for rows the operator
		// already cancelled but whose SubagentResult hasn't
		// cascaded in yet.
		if r.Cancelling {
			body = fmt.Sprintf("%s · ⊘ cancelling…", label)
		}
		line := fmt.Sprintf("%s%s  %s", leader, shortID(r.SessionID), body)
		style := missionRowStyle
		if i == state.selected {
			style = missionRowSelectedStyle
		}
		sb.WriteString(style.Render(truncate(line, contentW)))
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
	sb.WriteString(missionHintStyle.Render(
		"[j/k] select  [c/Enter] cancel  [Shift+C] cancel all  [esc] dismiss"))

	return missionBoxStyle.Width(width - 2).Render(sb.String())
}

// moveSelection clamps selected within [0, len(rows)-1]. No-op when
// the list is empty.
func (s *missionModalState) moveSelection(delta int) {
	if len(s.rows) == 0 {
		return
	}
	s.selected += delta
	if s.selected < 0 {
		s.selected = 0
	}
	if s.selected >= len(s.rows) {
		s.selected = len(s.rows) - 1
	}
}

// selectedRow returns the highlighted row plus a "row exists" flag.
// Returns (zero, false) for the empty-list case.
func (s *missionModalState) selectedRow() (missionRow, bool) {
	if len(s.rows) == 0 {
		return missionRow{}, false
	}
	if s.selected < 0 || s.selected >= len(s.rows) {
		return missionRow{}, false
	}
	return s.rows[s.selected], true
}

// markCancelling flips the Cancelling flag on the selected row so
// the next render shows "⊘ cancelling…" until the SubagentResult
// arrives and the liveview projection drops the child.
func (s *missionModalState) markCancelling(idx int) {
	if idx < 0 || idx >= len(s.rows) {
		return
	}
	s.rows[idx].Cancelling = true
}

// markAllCancelling flips Cancelling on every row — used by the
// Shift+C action so the operator sees the modal flash through the
// teardown collectively.
func (s *missionModalState) markAllCancelling() {
	for i := range s.rows {
		s.rows[i].Cancelling = true
	}
}

var (
	missionBoxStyle = lipgloss.NewStyle().
			BorderStyle(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("11")).
			Padding(0, 1)
	missionTitleStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("11"))
	missionFaintStyle       = lipgloss.NewStyle().Faint(true)
	missionHintStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))
	missionRowStyle         = lipgloss.NewStyle()
	missionRowSelectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
)
