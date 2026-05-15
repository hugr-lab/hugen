package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/hugr-lab/hugen/pkg/protocol"
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
	// Parked is true when the child sits in awaiting_dismissal.
	// Only parked rows accept `d` (dismiss) and `f` (follow-up).
	// Phase 5.2 ζ.
	Parked bool
	// ParkedAt mirrors liveviewStatus.ParkedAt so the modal can
	// render "⏸ parked Xs". Zero when not parked.
	ParkedAt time.Time
	// Dismissing flips true between the operator's `d` and the
	// runtime's teardown of a parked child — same role as
	// Cancelling but rendered as "✓ dismissing…" so the operator
	// can tell the two operations apart. Phase 5.2 ζ.
	Dismissing bool
}

// missionModalMode discriminates the modal's current input layer:
// the default "list" mode shows the rows + key hints; "followup"
// is the text-input substate the `f` key enters on a parked row.
// Phase 5.2 ζ.
type missionModalMode int

const (
	missionModeList missionModalMode = iota
	missionModeFollowup
)

// missionModalState carries everything the modal needs to render +
// dispatch. Lives on tab; mirrors the inquiry-modal pattern.
type missionModalState struct {
	rows     []missionRow
	selected int
	// Phase 5.2 ζ — follow-up text-input substate.
	mode           missionModalMode
	followupTarget string // session_id captured at mode entry
	followupRole   string // role of target — surfaced in the prompt
	followupBuf    string // current text typed by the operator
	// transientHint surfaces a row-level reject ("only parked
	// missions accept dismiss/follow-up") for one render cycle.
	// Cleared on the next valid key. Phase 5.2 ζ.
	transientHint string
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
		parked := c.LifecycleState == protocol.SessionStatusAwaitingDismissal
		out = append(out, missionRow{
			SessionID: id,
			Tier:      shortTierLabel(c.Depth),
			Role:      meta.Role,
			Goal:      strings.TrimSpace(meta.Task),
			StartedAt: meta.StartedAt,
			Parked:    parked,
			ParkedAt:  c.ParkedAt,
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

	// Phase 5.2 ζ — follow-up substate hijacks the body entirely.
	if state.mode == missionModeFollowup {
		return renderFollowupSubstate(state, width, contentW)
	}

	var sb strings.Builder
	sb.WriteString(missionTitleStyle.Render("Manage missions"))
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
		switch {
		case r.Parked && !r.ParkedAt.IsZero():
			age = " · " + missionParkedStyle.Render("⏸ parked "+ageString(r.ParkedAt))
		case r.Parked:
			age = " · " + missionParkedStyle.Render("⏸ parked")
		case !r.StartedAt.IsZero():
			age = " · " + ageString(r.StartedAt)
		}
		body := fmt.Sprintf("%s · %q%s", label, goal, age)
		switch {
		case r.Cancelling:
			body = fmt.Sprintf("%s · ⊘ cancelling…", label)
		case r.Dismissing:
			body = fmt.Sprintf("%s · ✓ dismissing…", label)
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
	if state.transientHint != "" {
		sb.WriteString(missionFaintStyle.Render(state.transientHint))
		sb.WriteString("\n")
	}
	sb.WriteString(missionHintStyle.Render(
		"[j/k] select  [c/Enter] cancel  [d] dismiss parked  [f] follow up  [Shift+C] cancel all  [esc] dismiss"))

	return missionBoxStyle.Width(width - 2).Render(sb.String())
}

// renderFollowupSubstate draws the small "type the directive" panel
// the modal switches to when `f` is pressed on a parked row. The
// operator types text; Enter submits, Esc returns to the row list.
// Phase 5.2 ζ.
func renderFollowupSubstate(state *missionModalState, width, contentW int) string {
	var sb strings.Builder
	sb.WriteString(missionTitleStyle.Render("Follow-up directive"))
	sb.WriteString("\n\n")
	label := "parked mission"
	if state.followupRole != "" {
		label = "parked " + state.followupRole
	}
	header := fmt.Sprintf("%s · %s", shortID(state.followupTarget), label)
	sb.WriteString(missionFaintStyle.Render(truncate(header, contentW)))
	sb.WriteString("\n\n")
	// Show the buffer with a trailing cursor block.
	body := state.followupBuf + "▌"
	sb.WriteString(missionFollowupInputStyle.Render(truncate(body, contentW)))
	sb.WriteString("\n\n")
	sb.WriteString(missionHintStyle.Render("[Enter] send  [esc] back  [ctrl+u] clear"))
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

// markDismissing is the dismiss counterpart of markCancelling.
// Phase 5.2 ζ.
func (s *missionModalState) markDismissing(idx int) {
	if idx < 0 || idx >= len(s.rows) {
		return
	}
	s.rows[idx].Dismissing = true
}

// enterFollowup transitions the modal into the follow-up text-input
// substate, capturing the target id + role for the prompt header.
// Phase 5.2 ζ.
func (s *missionModalState) enterFollowup(idx int) {
	if idx < 0 || idx >= len(s.rows) {
		return
	}
	r := s.rows[idx]
	s.mode = missionModeFollowup
	s.followupTarget = r.SessionID
	s.followupRole = r.Role
	s.followupBuf = ""
	s.transientHint = ""
}

// exitFollowup returns to the list view, discarding any typed
// directive. Phase 5.2 ζ.
func (s *missionModalState) exitFollowup() {
	s.mode = missionModeList
	s.followupTarget = ""
	s.followupRole = ""
	s.followupBuf = ""
}

// markAllCancelling flips Cancelling on every row — used by the
// Shift+C action so the operator sees the modal flash through the
// teardown collectively.
func (s *missionModalState) markAllCancelling() {
	for i := range s.rows {
		s.rows[i].Cancelling = true
	}
}

// rebuild refreshes the row list from a fresh liveview projection
// while preserving the Cancelling flag for any rows still present
// (mid-cancel feedback shouldn't blink off just because the
// liveview status frame raced the SubagentResult). Rows that
// vanish from the live projection (children that finished
// terminating) drop out; the selection clamps to the new bounds.
// Phase 5.x.skill-polish-1 — R1/R2 fix from cancel-ux review.
func (s *missionModalState) rebuild(live *liveviewStatus) {
	if s == nil {
		return
	}
	cancelling := make(map[string]bool, len(s.rows))
	dismissing := make(map[string]bool, len(s.rows))
	for _, r := range s.rows {
		if r.Cancelling {
			cancelling[r.SessionID] = true
		}
		if r.Dismissing {
			dismissing[r.SessionID] = true
		}
	}
	fresh := snapshotMissions(live)
	for i := range fresh {
		if cancelling[fresh[i].SessionID] {
			fresh[i].Cancelling = true
		}
		if dismissing[fresh[i].SessionID] {
			fresh[i].Dismissing = true
		}
	}
	s.rows = fresh
	if s.selected >= len(s.rows) {
		s.selected = max(0, len(s.rows)-1)
	}
	if s.selected < 0 {
		s.selected = 0
	}
	// Phase 5.2 ζ — if the follow-up target vanished from the
	// rebuild (mission completed / dismissed in the meantime), bail
	// out of the substate so the operator returns to the list
	// rather than typing into a dead handle.
	if s.mode == missionModeFollowup {
		found := false
		for _, r := range s.rows {
			if r.SessionID == s.followupTarget {
				found = true
				break
			}
		}
		if !found {
			s.exitFollowup()
			s.transientHint = "follow-up target is gone — returned to list"
		}
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
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
	missionParkedStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("13"))
	missionFollowupInputStyle = lipgloss.NewStyle().
					Padding(0, 1).
					Foreground(lipgloss.Color("15"))
)
