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
	// Phase 5.x — B15. Research-batch shape renders each
	// clarification as its own block with id, kind, question, and
	// optional pick-list.
	if len(state.req.Clarifications) > 0 {
		for _, c := range state.req.Clarifications {
			sb.WriteString("\n")
			label := fmt.Sprintf("[%s (%s)]", c.ID, kindOrDefault(c.Kind))
			sb.WriteString(inquiryFaintStyle.Render(label))
			sb.WriteString("\n")
			sb.WriteString(wrap(c.Question, contentW))
			sb.WriteString("\n")
			if len(c.Options) > 0 {
				sb.WriteString(inquiryFaintStyle.Render("  options:"))
				sb.WriteString("\n")
				for _, opt := range c.Options {
					sb.WriteString("    - ")
					sb.WriteString(truncate(opt, contentW-6))
					sb.WriteString("\n")
				}
			}
			if c.Default != "" {
				sb.WriteString(inquiryFaintStyle.Render(wrap("  default: "+c.Default, contentW)))
				sb.WriteString("\n")
			}
		}
		sb.WriteString("\n")
		sb.WriteString(inquiryFaintStyle.Render(wrap("Reply format: one line per question — `<id>: <value>` or `<id>: <value> | <comment>` (use `<id>: | <comment>` for comment-only answers).", contentW)))
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
	sb.WriteString(inquiryHintStyle.Render(actionHint(state)))

	return inquiryBoxStyle.Width(width - 2).Render(sb.String())
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
