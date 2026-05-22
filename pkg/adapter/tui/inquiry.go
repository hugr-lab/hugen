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
	//   - `inCommentPhase` flips on Tab; the textarea then
	//     captures the per-question comment instead of the value.
	//   - `answers` accumulates per-id entries as the operator
	//     advances. Built into payload.Answers on the last submit.
	currentIdx     int
	inCommentPhase bool
	answers        map[string]protocol.AnswerEntry
}

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

// commitBatchedValue stores value (or comment) for the current
// clarification, advances the cursor / phase, and returns true when
// the modal is ready to submit (last question answered).
func (s *inquiryState) commitBatchedValue(text string) (advanced bool, ready bool) {
	if !s.isBatched() {
		return false, false
	}
	if s.answers == nil {
		s.answers = make(map[string]protocol.AnswerEntry, len(s.req.Clarifications))
	}
	cur := s.currentClarification()
	entry := s.answers[cur.ID]
	if s.inCommentPhase {
		entry.Comment = text
	} else {
		// For comment-kind clarifications, treat any text as the
		// comment so the operator doesn't have to press Tab first.
		if cur.Kind == protocol.ClarificationKindComment {
			entry.Comment = text
		} else {
			entry.Value = text
		}
	}
	s.answers[cur.ID] = entry
	s.inCommentPhase = false
	if s.currentIdx+1 >= len(s.req.Clarifications) {
		return true, true
	}
	s.currentIdx++
	return true, false
}

// resolveOptionPick returns (matched-option-text, true) when `text`
// is a digit selecting one of the current clarification's options
// (`1` → options[0], etc.). Returns ("", false) otherwise so the
// caller treats the raw text as the answer.
func (s *inquiryState) resolveOptionPick(text string) (string, bool) {
	if !s.isBatched() || s.inCommentPhase {
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
	// Only treat as a pick when the whole input was the number
	// (or "1." style). Free-text answers that happen to start with
	// a digit (e.g. "30 days") still pass through verbatim.
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
// modal for research_batch inquiries. Shows progress + current
// question + numbered options + accumulated previous answers as
// a faint summary.
func renderBatchedInquiryModal(state *inquiryState, width, contentW int) string {
	total := len(state.req.Clarifications)
	cur := state.currentClarification()

	var sb strings.Builder
	title := fmt.Sprintf("Research clarifications [%d/%d]", state.currentIdx+1, total)
	if state.callerLabel != "" {
		title += " (from " + state.callerLabel + ")"
	}
	sb.WriteString(inquiryTitleStyle.Render(title))
	sb.WriteString("\n\n")

	// Optional rationale / context line that the request payload
	// carried (e.g. "Re-approval requested: ..." for approval but
	// also surfaces research-stage notes when set).
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

	// Numbered option list when the role provided picks.
	if len(cur.Options) > 0 {
		sb.WriteString("\n")
		sb.WriteString(inquiryFaintStyle.Render("options:"))
		sb.WriteString("\n")
		for i, opt := range cur.Options {
			sb.WriteString(fmt.Sprintf("  %d. ", i+1))
			sb.WriteString(truncate(opt, contentW-6))
			sb.WriteString("\n")
		}
	}
	if cur.Default != "" {
		sb.WriteString(inquiryFaintStyle.Render(wrap("  default: "+cur.Default, contentW)))
		sb.WriteString("\n")
	}

	// Phase indicator + previously-captured value/comment for this
	// question if the operator has already touched it (e.g. typed
	// a value, then pressed Tab to add a comment).
	if entry, ok := state.answers[cur.ID]; ok {
		sb.WriteString("\n")
		if entry.Value != "" {
			sb.WriteString(inquiryFaintStyle.Render(wrap("  value: "+entry.Value, contentW)))
			sb.WriteString("\n")
		}
		if entry.Comment != "" {
			sb.WriteString(inquiryFaintStyle.Render(wrap("  comment: "+entry.Comment, contentW)))
			sb.WriteString("\n")
		}
	}

	// Phase + hint line.
	phase := "value"
	if state.inCommentPhase || cur.Kind == protocol.ClarificationKindComment {
		phase = "comment"
	}
	sb.WriteString("\n")
	sb.WriteString(inquiryFaintStyle.Render(fmt.Sprintf("Now editing: %s", phase)))
	sb.WriteString("\n")
	sb.WriteString(inquiryHintStyle.Render(batchedHint(state)))

	return inquiryBoxStyle.Width(width - 2).Render(sb.String())
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
	if state.inCommentPhase {
		return fmt.Sprintf("[type comment, Enter to %s | Tab back to value | Esc abort]", enterAction)
	}
	if cur.Kind == protocol.ClarificationKindComment {
		return fmt.Sprintf("[type free-form, Enter to %s | Esc abort]", enterAction)
	}
	if len(cur.Options) > 0 {
		return fmt.Sprintf("[type 1-%d to pick OR free text, Enter to %s | Tab add comment | Esc abort]", len(cur.Options), enterAction)
	}
	return fmt.Sprintf("[type answer, Enter to %s | Tab add comment | Esc abort]", enterAction)
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
