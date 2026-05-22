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
type PendingInquiry struct {
	RequestID       string
	CallerSessionID string
	Kind            string // protocol.InquiryType{Approval,Clarification,ResearchBatch}
	// ClarificationIDs preserves the ordered set of clarification
	// IDs from the inbound request payload so the reply parser
	// (research_batch shape) can validate the user typed an id
	// the role asked about. Empty for legacy single-question
	// shapes. Phase 5.x — B15.
	ClarificationIDs []string
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
	case protocol.InquiryTypeResearchBatch:
		answers, err := parseResearchBatchReply(line, pend.ClarificationIDs)
		if err != nil {
			return nil, err
		}
		payload.Answers = answers
	default:
		return nil, fmt.Errorf("unknown inquiry type %q", pend.Kind)
	}
	return protocol.NewInquiryResponse(rootSessionID, user, payload), nil
}

// parseResearchBatchReply parses the operator's batched reply
// into the typed answers map. Each line is `<id>: <value>` or
// `<id>: <value> | <comment>` or `<id>: | <comment>` (comment-
// only). Empty lines and lines starting with `#` are skipped so
// the user can paste structured text with comments.
//
// Validation: every id MUST be in knownIDs (passed in from the
// pending state) — typos surface as errors so the user can fix
// without the runtime silently dropping an answer.
func parseResearchBatchReply(line string, knownIDs []string) (map[string]protocol.AnswerEntry, error) {
	if strings.TrimSpace(line) == "" {
		return nil, fmt.Errorf("research_batch needs at least one `<id>: <value>` line")
	}
	known := make(map[string]struct{}, len(knownIDs))
	for _, id := range knownIDs {
		known[id] = struct{}{}
	}
	out := make(map[string]protocol.AnswerEntry, len(knownIDs))
	for _, raw := range strings.Split(line, "\n") {
		l := strings.TrimSpace(raw)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		idPart, body, ok := strings.Cut(l, ":")
		if !ok {
			return nil, fmt.Errorf("line %q: expected `<id>: <value>` shape", l)
		}
		id := strings.TrimSpace(idPart)
		if id == "" {
			return nil, fmt.Errorf("line %q: empty id", l)
		}
		if len(known) > 0 {
			if _, present := known[id]; !present {
				return nil, fmt.Errorf("line %q: id %q was not in the asked clarifications", l, id)
			}
		}
		body = strings.TrimSpace(body)
		value, comment, _ := strings.Cut(body, "|")
		entry := protocol.AnswerEntry{
			Value:   strings.TrimSpace(value),
			Comment: strings.TrimSpace(comment),
		}
		if entry.Value == "" && entry.Comment == "" {
			return nil, fmt.Errorf("line %q: provide a value, a `| comment`, or both", l)
		}
		out[id] = entry
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("research_batch reply parsed to zero answers — at least one is required")
	}
	return out, nil
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
