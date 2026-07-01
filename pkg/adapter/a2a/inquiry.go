package a2a

import (
	"fmt"
	"strings"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// buildInquiryResponse turns the user's inbound answer text into an
// InquiryResponse addressed to the root session. It is the A2A analogue of
// tui.BuildInquiryReply — the adapter-agnostic core of the HITL round-trip —
// but tuned for a chat transport: there are no slash-command affordances, so an
// approval answer that is neither a clear approve nor a clear deny is treated as
// refinement guidance (Approved=nil + Response=text) rather than rejected. The
// mission ext folds that free-form text into the planner's next iteration, so
// the user is never dead-ended by phrasing.
//
// The response is addressed to rootSessionID (where the adapter observed the
// request); root's handler cascades it down the parent chain to whichever tier
// actually called session:inquire, keyed by CallerSessionID. Spec §A5.
func buildInquiryResponse(user protocol.ParticipantInfo, rootSessionID string, pend *parkedInquiry, text string) (*protocol.InquiryResponse, error) {
	payload := protocol.InquiryResponsePayload{
		RequestID:       pend.RequestID,
		CallerSessionID: pend.CallerSessionID,
		RespondedAt:     time.Now().UTC().Format(time.RFC3339Nano),
	}
	answer := strings.TrimSpace(text)
	switch pend.Kind {
	case protocol.InquiryTypeApproval:
		if answer == "" {
			return nil, fmt.Errorf("approval needs 'approve' / 'deny' (or feedback to refine)")
		}
		if approved, withTools, reason, ok := parseApprovalAnswer(answer); ok {
			payload.Approved = &approved
			payload.Reason = reason
			payload.AutoApproveTools = withTools
		} else {
			// Free-form text on an approval inquiry = refinement guidance
			// (matches the TUI's §4.6 "refine" path: Approved=nil + Response).
			payload.Response = answer
		}
	case protocol.InquiryTypeClarification, protocol.InquiryTypeResearchBatch:
		// Clarification (and, best-effort, a single-line research_batch answer):
		// the whole line is the response.
		//
		// KNOWN LIMITATION (M3): a research_batch carries Clarifications[] keyed
		// by stable id and the research role reads answers back from
		// Answers[id]; collapsing to a single free-form Response means the
		// research stage gets no structured answers over A2A. inquiryPrompt does
		// enumerate the questions so a human can read them, but the reply isn't
		// mapped per-id. research_batch over A2A is effectively unsupported until
		// a per-id answer surface is added; single-question clarification (the
		// common case) works.
		if answer == "" {
			return nil, fmt.Errorf("a clarification needs a non-empty answer")
		}
		payload.Response = answer
	default:
		return nil, fmt.Errorf("unknown inquiry type %q", pend.Kind)
	}
	return protocol.NewInquiryResponse(rootSessionID, user, payload), nil
}

// parseApprovalAnswer recognises the approve/deny verdict (and the "approve with
// tools" escalation) at the head of an answer line. ok=false means the text is
// not a recognised verdict and the caller should treat it as free-form
// refinement. The remainder after the verb is captured as the reason.
func parseApprovalAnswer(line string) (approved, withTools bool, reason string, ok bool) {
	verb, rest := splitFirstWord(strings.TrimPrefix(strings.TrimSpace(line), "/"))
	switch strings.ToLower(verb) {
	case "approve", "approved", "yes", "y", "ok":
		// "approve with tools" / "approve tools" / "approve all" auto-approves
		// every requires_approval tool under this mission (§4.6 with-tools row).
		low := strings.ToLower(rest)
		if strings.HasPrefix(low, "with tools") || strings.HasPrefix(low, "tools") || strings.HasPrefix(low, "all") {
			return true, true, "", true
		}
		return true, false, rest, true
	case "deny", "denied", "no", "n", "reject", "rejected":
		return false, false, rest, true
	default:
		return false, false, "", false
	}
}

// splitFirstWord peels the first space-delimited token off s; the remainder
// (leading whitespace trimmed) is rest. The a2a-local twin of tui's
// splitFirstToken — duplicated rather than shared to keep the adapters free of
// cross-imports.
func splitFirstWord(s string) (head, rest string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	head, rest, _ = strings.Cut(s, " ")
	return head, strings.TrimSpace(rest)
}

// inquiryPrompt renders the human-readable prompt the input-required task
// carries: any assistant text streamed before the question (preamble), then the
// question, the detail Context (plan / acceptance criteria on an approval), and
// enumerated options or batched sub-questions when present. It closes an
// approval prompt with the reply grammar so a chat user knows how to answer.
func inquiryPrompt(p *protocol.InquiryRequestPayload, preamble string) string {
	var b strings.Builder
	if pre := strings.TrimSpace(preamble); pre != "" {
		b.WriteString(pre)
		b.WriteString("\n\n")
	}
	if q := strings.TrimSpace(p.Question); q != "" {
		b.WriteString(q)
	} else {
		b.WriteString(defaultInquiryQuestion(p.Type))
	}
	if c := strings.TrimSpace(p.Context); c != "" {
		b.WriteString("\n\n")
		b.WriteString(c)
	}
	if len(p.Clarifications) > 0 {
		b.WriteString("\n")
		for i, c := range p.Clarifications {
			b.WriteString(fmt.Sprintf("\n%d. %s", i+1, strings.TrimSpace(c.Question)))
		}
	} else if len(p.Options) > 0 {
		b.WriteString("\n")
		for _, opt := range p.Options {
			b.WriteString("\n  - ")
			b.WriteString(strings.TrimSpace(opt))
		}
	}
	if p.Type == protocol.InquiryTypeApproval {
		b.WriteString("\n\nReply 'approve' (or 'approve with tools'), 'deny <reason>', or type feedback to refine.")
	}
	return strings.TrimSpace(b.String())
}

// defaultInquiryQuestion is the fallback prompt when an inquiry carries no
// Question text of its own.
func defaultInquiryQuestion(typ string) string {
	switch typ {
	case protocol.InquiryTypeApproval:
		return "Approval required."
	default:
		return "Input required."
	}
}
