package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// inquiryState is the TUI's view of a live [protocol.InquiryRequest]
// while waiting for the operator's answer. Set when the frame lands,
// cleared on the echo [protocol.InquiryResponse] or on
// [protocol.SessionTerminated]. Slice 3 — modal overlay UX per
// phase-5.1c §7.
type inquiryState struct {
	req protocol.InquiryRequestPayload
	// callerLabel is a short "from <session>" hint shown in the
	// modal title. Resolved from req.CallerSessionID; empty when
	// the inquiry originates from root.
	callerLabel string
	// replyMode flips true when the operator presses `r` on an
	// approval inquiry; the textarea below the modal is focused and
	// the next Enter submits "/approve <reason>" or "/deny <reason>".
	// For clarifications replyMode is always true (the textarea is
	// the only input path).
	replyMode bool
	// replyVerb is the approval verdict the typed reason will be
	// stitched onto: "approve" or "deny". Empty for clarifications.
	replyVerb string

	// Phase 5.x — B15. Tab-style batched-research state. Modal
	// walks the operator through clarifications one at a time:
	//   - `currentIdx` names the clarification being asked.
	//   - `focus` selects which input field has the textarea: 0
	//     for value, 1 for comment. Tab toggles. (For comment-
	//     kind clarifications focus is locked at 1 because there
	//     IS no value field.)
	//   - `optionHighlights` caches each option-bearing question's
	//     cursor index — what ↑/↓ moved to. Persists across
	//     navigation so a back-walk doesn't lose the prior pick.
	//     `-1` means "no option highlighted yet" (default before
	//     the operator presses ↑/↓).
	//   - `answers` accumulates per-id entries as the operator
	//     advances. Built into payload.Answers on the last submit.
	currentIdx       int
	focus            int
	optionHighlights map[string]int
	answers          map[string]protocol.AnswerEntry
}

// Field focus identifiers for the batched modal.
const (
	batchedFocusValue   = 0
	batchedFocusComment = 1
)

// isBatched reports whether this state is rendering a research_batch
// modal. Used to switch the modal renderer + key dispatch into the
// one-question-at-a-time tab walk.
func (s *inquiryState) isBatched() bool {
	return s != nil && s.req.Type == protocol.InquiryTypeResearchBatch && len(s.req.Clarifications) > 0
}

// currentClarification returns the clarification the operator is
// currently answering. Safe to call only when isBatched() returns
// true.
func (s *inquiryState) currentClarification() protocol.Clarification {
	if s.currentIdx < 0 || s.currentIdx >= len(s.req.Clarifications) {
		return protocol.Clarification{}
	}
	return s.req.Clarifications[s.currentIdx]
}

// optionHighlight returns the cached option index for the current
// clarification (-1 when the operator hasn't pressed ↑/↓ on this
// question yet). Used by the renderer to mark the highlighted row
// and by the commit path to use the highlight as the value when
// the operator left the value textarea empty.
func (s *inquiryState) optionHighlight() int {
	if s.optionHighlights == nil {
		return -1
	}
	idx, ok := s.optionHighlights[s.currentClarification().ID]
	if !ok {
		return -1
	}
	return idx
}

// setOptionHighlight caches the cursor index for the current
// clarification.
func (s *inquiryState) setOptionHighlight(idx int) {
	if s.optionHighlights == nil {
		s.optionHighlights = make(map[string]int)
	}
	s.optionHighlights[s.currentClarification().ID] = idx
}

// cycleOptionHighlight advances (delta=+1) or rolls back (delta=-1)
// the current question's option highlight, wrapping at the bounds.
// No-op when the question has no options.
func (s *inquiryState) cycleOptionHighlight(delta int) {
	cur := s.currentClarification()
	if len(cur.Options) == 0 {
		return
	}
	idx := s.optionHighlight()
	if idx < 0 {
		// First ↑/↓ press selects the first option for ↓, last for
		// ↑ — matches list-widget conventions.
		if delta > 0 {
			idx = 0
		} else {
			idx = len(cur.Options) - 1
		}
		s.setOptionHighlight(idx)
		return
	}
	idx = (idx + delta + len(cur.Options)) % len(cur.Options)
	s.setOptionHighlight(idx)
}

// advanceBatched moves the cursor to the next question. Returns
// true when the modal is ready to submit (last question committed).
func (s *inquiryState) advanceBatched() bool {
	if s.currentIdx+1 >= len(s.req.Clarifications) {
		return true
	}
	s.currentIdx++
	// Reset focus: comment-kind questions force focus to the
	// comment field (the only one rendered); everyone else
	// defaults to value.
	if s.currentClarification().Kind == protocol.ClarificationKindComment {
		s.focus = batchedFocusComment
	} else {
		s.focus = batchedFocusValue
	}
	return false
}

// resolveOptionPick returns (matched-option-text, true) when `text`
// is a digit selecting one of the current clarification's options
// (`1` → options[0], etc.). Returns ("", false) otherwise so the
// caller treats the raw text as the answer. Kept as a power-user
// shortcut alongside the ↑/↓ + Enter flow.
func (s *inquiryState) resolveOptionPick(text string) (string, bool) {
	if !s.isBatched() || s.focus == batchedFocusComment {
		return "", false
	}
	cur := s.currentClarification()
	if len(cur.Options) == 0 {
		return "", false
	}
	t := strings.TrimSpace(text)
	if t == "" {
		return "", false
	}
	if t[0] < '1' || t[0] > '9' {
		return "", false
	}
	idx := int(t[0]-'0') - 1
	if idx < 0 || idx >= len(cur.Options) {
		return "", false
	}
	rest := strings.TrimSpace(t[1:])
	if rest != "" && rest != "." {
		return "", false
	}
	return cur.Options[idx], true
}

// newInquiryState builds the modal handle from an inbound request
// frame. Auto-enters reply mode for clarifications + research_batch
// since they have no keystroke shortcuts.
func newInquiryState(req *protocol.InquiryRequest) *inquiryState {
	s := &inquiryState{
		req:         req.Payload,
		callerLabel: shortID(req.Payload.CallerSessionID),
	}
	switch req.Payload.Type {
	case protocol.InquiryTypeClarification, protocol.InquiryTypeResearchBatch:
		s.replyMode = true
	}
	// Phase 5.x — B15. Initialise focus for the batched walk:
	// comment-kind first question locks focus on the comment
	// field; everyone else starts on the value field.
	if s.isBatched() {
		if s.currentClarification().Kind == protocol.ClarificationKindComment {
			s.focus = batchedFocusComment
		} else {
			s.focus = batchedFocusValue
		}
	}
	return s
}

// title produces the modal header.
func (s *inquiryState) title() string {
	switch s.req.Type {
	case protocol.InquiryTypeApproval:
		if s.callerLabel != "" {
			return "Approval required (from " + s.callerLabel + ")"
		}
		return "Approval required"
	case protocol.InquiryTypeClarification:
		if s.callerLabel != "" {
			return "Clarification needed (from " + s.callerLabel + ")"
		}
		return "Clarification needed"
	case protocol.InquiryTypeResearchBatch:
		if s.callerLabel != "" {
			return "Research clarifications (from " + s.callerLabel + ")"
		}
		return "Research clarifications"
	default:
		return "Inquiry: " + s.req.Type
	}
}

// renderInquiryModal returns the rendered modal block sized to
// width. Height is determined by content; callers JoinVertical the
// result over chat. Caller must guarantee state != nil.
func renderInquiryModal(state *inquiryState, width int) string {
	if width < 30 {
		width = 30
	}
	contentW := width - 4 // border + padding
	if contentW < 10 {
		contentW = 10
	}

	// Tab-style batched flow: one clarification at a time, with
	// per-question progress indicator + numbered options pick-list
	// + per-question comment phase (toggled via Tab). Phase 5.x
	// — B15.
	if state.isBatched() {
		return renderBatchedInquiryModal(state, width, contentW)
	}

	var sb strings.Builder
	sb.WriteString(inquiryTitleStyle.Render(state.title()))
	sb.WriteString("\n\n")
	if q := strings.TrimSpace(state.req.Question); q != "" {
		sb.WriteString(wrap(q, contentW))
		sb.WriteString("\n")
	}
	if c := strings.TrimSpace(state.req.Context); c != "" {
		sb.WriteString("\n")
		sb.WriteString(inquiryFaintStyle.Render(wrap("Context: "+c, contentW)))
		sb.WriteString("\n")
	}
	if len(state.req.Options) > 0 {
		sb.WriteString("\n")
		sb.WriteString(inquiryFaintStyle.Render("Options:"))
		sb.WriteString("\n")
		for _, opt := range state.req.Options {
			sb.WriteString("  - ")
			sb.WriteString(truncate(opt, contentW-4))
			sb.WriteString("\n")
		}
	}
	sb.WriteString("\n")
	sb.WriteString(inquiryHintStyle.Render(actionHint(state)))

	return inquiryBoxStyle.Width(width - 2).Render(sb.String())
}

// renderBatchedInquiryModal renders the tab-style one-at-a-time
// modal for research_batch inquiries. Per question: progress bar,
// question header, option list (with ↑↓-driven highlight marker),
// always-visible `value` and `comment` rows (focused one carries
// the cursor textarea content), and a keystroke hint footer.
func renderBatchedInquiryModal(state *inquiryState, width, contentW int) string {
	total := len(state.req.Clarifications)
	cur := state.currentClarification()
	commentKind := cur.Kind == protocol.ClarificationKindComment

	var sb strings.Builder
	title := fmt.Sprintf("Research clarifications [%d/%d]", state.currentIdx+1, total)
	if state.callerLabel != "" {
		title += " (from " + state.callerLabel + ")"
	}
	sb.WriteString(inquiryTitleStyle.Render(title))
	sb.WriteString("\n\n")

	// Optional rationale / context line that the request payload
	// carried.
	if c := strings.TrimSpace(state.req.Context); c != "" {
		sb.WriteString(inquiryFaintStyle.Render(wrap("Context: "+c, contentW)))
		sb.WriteString("\n\n")
	}

	// Header for the current question.
	label := fmt.Sprintf("%s (%s)", cur.ID, kindOrDefault(cur.Kind))
	sb.WriteString(inquiryFaintStyle.Render(label))
	sb.WriteString("\n")
	sb.WriteString(wrap(cur.Question, contentW))
	sb.WriteString("\n")

	// Option list with ▸ on the currently-highlighted row. Numbered
	// (1., 2., ...) so digit-select still works as a shortcut.
	if len(cur.Options) > 0 {
		sb.WriteString("\n")
		sb.WriteString(inquiryFaintStyle.Render("options (↑↓ to pick, Enter on value field to commit):"))
		sb.WriteString("\n")
		hi := state.optionHighlight()
		for i, opt := range cur.Options {
			marker := "  "
			if i == hi {
				marker = "▸ "
			}
			line := fmt.Sprintf("%s%d. %s", marker, i+1, truncate(opt, contentW-6))
			if i == hi {
				sb.WriteString(inquiryHintStyle.Render(line))
			} else {
				sb.WriteString(line)
			}
			sb.WriteString("\n")
		}
	}
	if cur.Default != "" {
		sb.WriteString(inquiryFaintStyle.Render(wrap("  default: "+cur.Default, contentW)))
		sb.WriteString("\n")
	}

	// Value + Comment rows. The currently-focused one is marked
	// with `>`; its textarea content is shown inline (the real
	// textarea component below the modal mirrors this — keeping
	// the modal preview honest). Comment-kind questions hide the
	// value row entirely (the question IS the comment).
	sb.WriteString("\n")
	entry := state.answers[cur.ID]
	if !commentKind {
		sb.WriteString(renderBatchedField(
			"value",
			state.focus == batchedFocusValue,
			fieldValuePreview(state, cur, entry),
			contentW,
		))
		sb.WriteString("\n")
	}
	sb.WriteString(renderBatchedField(
		"comment",
		state.focus == batchedFocusComment,
		fieldCommentPreview(state, cur, entry),
		contentW,
	))
	sb.WriteString("\n\n")

	sb.WriteString(inquiryHintStyle.Render(batchedHint(state)))
	return inquiryBoxStyle.Width(width - 2).Render(sb.String())
}

// renderBatchedField renders one labelled field row. The focused
// row gets a `>` marker + bright color; unfocused rows render
// faint to keep the eye on the active input.
func renderBatchedField(label string, focused bool, preview string, contentW int) string {
	marker := "  "
	style := inquiryFaintStyle
	if focused {
		marker = "> "
		style = inquiryHintStyle
	}
	line := fmt.Sprintf("%s%s: %s", marker, label, truncate(preview, contentW-len(label)-4))
	return style.Render(line)
}

// fieldValuePreview returns the inline preview text for the value
// row. When the value field has focus, show what the operator is
// currently typing (mirrored from the real textarea by the caller —
// we receive it through the state's last-known content). Otherwise
// show the committed value, the highlighted option, or "—".
//
// Today we don't mirror the live textarea content into state on
// every keystroke (the textarea owns its own buffer); the preview
// falls back to the persisted entry. The hint footer still makes
// it clear which field is active.
func fieldValuePreview(state *inquiryState, cur protocol.Clarification, entry protocol.AnswerEntry) string {
	if entry.Value != "" {
		return entry.Value
	}
	if hi := state.optionHighlight(); hi >= 0 && hi < len(cur.Options) {
		return inquiryFaintStyle.Render("(↑↓ highlighted: " + cur.Options[hi] + ")")
	}
	if state.focus == batchedFocusValue {
		return inquiryFaintStyle.Render("(type to fill)")
	}
	return inquiryFaintStyle.Render("—")
}

// fieldCommentPreview mirrors fieldValuePreview for the comment row.
func fieldCommentPreview(state *inquiryState, _ protocol.Clarification, entry protocol.AnswerEntry) string {
	if entry.Comment != "" {
		return entry.Comment
	}
	if state.focus == batchedFocusComment {
		return inquiryFaintStyle.Render("(type to fill)")
	}
	return inquiryFaintStyle.Render("—")
}

// batchedHint is the one-line keystroke summary at the bottom of
// the batched modal.
func batchedHint(state *inquiryState) string {
	cur := state.currentClarification()
	last := state.currentIdx+1 == len(state.req.Clarifications)
	enterAction := "next"
	if last {
		enterAction = "submit"
	}
	backHint := ""
	if state.currentIdx > 0 {
		backHint = " | Shift+Tab prev"
	}
	if cur.Kind == protocol.ClarificationKindComment {
		return fmt.Sprintf("[type comment, Enter %s%s | Esc abort]", enterAction, backHint)
	}
	parts := []string{}
	if len(cur.Options) > 0 {
		parts = append(parts, "↑↓ pick option")
	}
	parts = append(parts, "Tab switch value↔comment")
	parts = append(parts, fmt.Sprintf("Enter %s", enterAction))
	if backHint != "" {
		parts = append(parts, strings.TrimSpace(strings.TrimPrefix(backHint, " | ")))
	}
	parts = append(parts, "Esc abort")
	return "[" + strings.Join(parts, " | ") + "]"
}

// kindOrDefault returns kind, defaulting to "required" so a
// missing/empty Kind doesn't render as the empty string.
func kindOrDefault(k string) string {
	if k == "" {
		return protocol.ClarificationKindRequired
	}
	return k
}

// actionHint is the bottom action line — kept tight (single line)
// so the modal stays vertical-budget friendly.
func actionHint(s *inquiryState) string {
	switch s.req.Type {
	case protocol.InquiryTypeApproval:
		if s.replyMode {
			verb := s.replyVerb
			if verb == "" {
				verb = "approve"
			}
			return fmt.Sprintf("[type reason, Enter to /%s | esc to cancel]", verb)
		}
		return "[y] approve  [n] deny  [r] reply with reason  [esc] dismiss"
	case protocol.InquiryTypeClarification:
		return "[type answer, Enter to submit | esc to dismiss]"
	case protocol.InquiryTypeResearchBatch:
		return "[type one line per question, Enter to submit | esc to dismiss]"
	}
	return "[esc to dismiss]"
}

// wrap is a minimal word-wrapper for the modal body. Splits on
// whitespace, packs words into lines ≤ width. Existing newlines in
// the input are honoured (each paragraph wrapped independently).
func wrap(s string, width int) string {
	if width <= 0 {
		return s
	}
	var out strings.Builder
	for i, para := range strings.Split(s, "\n") {
		if i > 0 {
			out.WriteString("\n")
		}
		var line strings.Builder
		for _, w := range strings.Fields(para) {
			if line.Len() == 0 {
				line.WriteString(w)
				continue
			}
			if line.Len()+1+len(w) > width {
				out.WriteString(line.String())
				out.WriteString("\n")
				line.Reset()
				line.WriteString(w)
				continue
			}
			line.WriteString(" ")
			line.WriteString(w)
		}
		if line.Len() > 0 {
			out.WriteString(line.String())
		}
	}
	return out.String()
}

var (
	inquiryBoxStyle = lipgloss.NewStyle().
			BorderStyle(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("11")).
			Padding(0, 1)
	inquiryTitleStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("11"))
	inquiryFaintStyle = lipgloss.NewStyle().Faint(true)
	inquiryHintStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))
)
