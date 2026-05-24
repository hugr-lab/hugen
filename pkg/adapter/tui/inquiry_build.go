package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// PendingInquiry is the user-side state of an in-flight HITL
// inquiry — the runtime emitted an [protocol.InquiryRequest], the
// adapter rendered it, and now waits for the user to answer.
// Phase 5.1 — adapters own the user-facing leg of the inquiry
// round trip; the runtime stays UI-agnostic.
//
// Research-batch shape (Phase 5.x — B15) does NOT route through
// BuildInquiryReply; the TUI's tab-style modal builds the typed
// Answers map directly via tab.submitBatchedAnswers. Only Approval
// + Clarification kinds reach this struct.
type PendingInquiry struct {
	RequestID       string
	CallerSessionID string
	Kind            string // protocol.InquiryType{Approval,Clarification}
}

// BuildInquiryReply turns the raw input line into an
// [protocol.InquiryResponse] frame addressed at the root session id.
// The CallerSessionID from the original request payload is preserved
// so dispatchInquiryResponse cascades the answer back down the
// parent chain to whichever tier actually called session:inquire.
//
// Standalone helper (no receiver) so unit tests + multiple adapters
// can reuse it without bringing the session manager up.
func BuildInquiryReply(user protocol.ParticipantInfo, rootSessionID string, pend *PendingInquiry, line string) (*protocol.InquiryResponse, error) {
	payload := protocol.InquiryResponsePayload{
		RequestID:       pend.RequestID,
		CallerSessionID: pend.CallerSessionID,
		RespondedAt:     time.Now().UTC().Format(time.RFC3339Nano),
	}
	switch pend.Kind {
	case protocol.InquiryTypeApproval:
		approved, reason, err := parseApprovalReply(line)
		if err != nil {
			return nil, err
		}
		payload.Approved = &approved
		payload.Reason = reason
	case protocol.InquiryTypeClarification:
		text, err := parseClarificationReply(line)
		if err != nil {
			return nil, err
		}
		payload.Response = text
	// Phase 5.x — B15. ResearchBatch replies are constructed
	// directly by the TUI's tab-style modal via
	// tab.submitBatchedAnswers — BuildInquiryReply does NOT cover
	// this kind. Routing it here is a bug; surface loud.
	default:
		return nil, fmt.Errorf("unknown inquiry type %q", pend.Kind)
	}
	return protocol.NewInquiryResponse(rootSessionID, user, payload), nil
}

// parseApprovalReply recognises /approve, /deny, and the bare
// yes/no shorthand. Anything that follows the verdict keyword is
// captured as the reason (the protocol carries Reason for both
// directions — useful audit trail for "denied: not safe to run").
func parseApprovalReply(line string) (approved bool, reason string, err error) {
	t := strings.TrimSpace(line)
	if t == "" {
		return false, "", fmt.Errorf("approval needs /approve|/deny|yes|no")
	}
	verdict, rest := splitFirstToken(strings.TrimPrefix(t, "/"))
	switch strings.ToLower(verdict) {
	case "approve", "yes", "y", "ok":
		return true, rest, nil
	case "deny", "no", "n":
		return false, rest, nil
	default:
		return false, "", fmt.Errorf("approval needs /approve|/deny|yes|no (got %q)", verdict)
	}
}

// parseClarificationReply accepts /respond <text> or a bare
// non-empty line. Empty input is rejected so the user can correct
// without locking us into an empty Response.
func parseClarificationReply(line string) (string, error) {
	t := strings.TrimSpace(line)
	if t == "" {
		return "", fmt.Errorf("clarification needs a non-empty answer")
	}
	if strings.HasPrefix(t, "/respond") {
		t = strings.TrimSpace(strings.TrimPrefix(t, "/respond"))
		if t == "" {
			return "", fmt.Errorf("/respond needs a non-empty answer")
		}
	}
	return t, nil
}

// splitFirstToken peels off the first space-delimited token from
// s; the remainder (with leading whitespace trimmed) is returned
// as rest. Pure helper — no quote handling, no tab handling
// (CLI input is space-separated in practice).
func splitFirstToken(s string) (head, rest string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	head, rest, _ = strings.Cut(s, " ")
	return head, strings.TrimSpace(rest)
}
