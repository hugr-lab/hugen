package console

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// PendingInquiry is the live HITL request the console is waiting
// for the user to answer. Set when an *protocol.InquiryRequest
// frame lands in render; consumed in runInput on the next user
// line. Phase 5.1 § 2 — the adapter owns the user-facing leg of
// the inquiry round trip.
type PendingInquiry struct {
	RequestID       string
	CallerSessionID string
	Kind            string // protocol.InquiryType{Approval,Clarification}
}

// renderInquiryRequest draws the HITL prompt block for an
// inbound inquiry and stashes it as the pending reply target.
// Caller holds a.mu.
func (a *Adapter) renderInquiryRequest(req *protocol.InquiryRequest) {
	if a.currentSection != "" {
		fmt.Fprintln(a.out)
		a.currentSection = ""
	}
	p := req.Payload
	fmt.Fprintf(a.out, "─── HITL: %s ───\n", p.Type)
	if q := strings.TrimSpace(p.Question); q != "" {
		fmt.Fprintf(a.out, "Q: %s\n", q)
	}
	if c := strings.TrimSpace(p.Context); c != "" {
		fmt.Fprintf(a.out, "Context: %s\n", c)
	}
	if len(p.Options) > 0 {
		fmt.Fprintln(a.out, "Options:")
		for _, opt := range p.Options {
			fmt.Fprintf(a.out, "  - %s\n", opt)
		}
	}
	switch p.Type {
	case protocol.InquiryTypeApproval:
		fmt.Fprintln(a.out, "[reply: /approve [reason] | /deny [reason] | yes | no]")
	default:
		fmt.Fprintln(a.out, "[reply: any text — or /respond <text>]")
	}
	a.pending.Store(&PendingInquiry{
		RequestID:       p.RequestID,
		CallerSessionID: p.CallerSessionID,
		Kind:            p.Type,
	})
	fmt.Fprint(a.out, "> ")
}

// maybeHandleInquiryReply attempts to interpret the line as an
// answer to the pending inquiry. Returns true when the line was
// fully consumed (either submitted or rejected with a hint);
// returns false when the line should fall through to normal
// slash-command / user-message handling (e.g. /end, /help).
func (a *Adapter) maybeHandleInquiryReply(ctx context.Context, pend *PendingInquiry, line string) bool {
	t := strings.TrimSpace(line)
	if IsSlashCommand(t) {
		pc := ParseSlashCommand(t)
		switch pc.Name {
		case "approve", "deny", "respond":
			// fall through into reply build
		default:
			return false
		}
	}
	resp, err := BuildInquiryReply(a.user, a.session.ID(), pend, t)
	if err != nil {
		fmt.Fprintf(a.err, "inquiry reply: %v\n", err)
		fmt.Fprint(a.out, "> ")
		return true
	}
	if subErr := a.host.Submit(ctx, resp); subErr != nil {
		fmt.Fprintf(a.err, "submit inquiry: %v\n", subErr)
		fmt.Fprint(a.out, "> ")
		return true
	}
	a.pending.Store(nil)
	return true
}

// BuildInquiryReply turns the raw input line into an
// InquiryResponse Frame addressed at the root session id. The
// CallerSessionID from the original request payload is preserved
// so dispatchInquiryResponse cascades the answer back down the
// parent chain to whichever tier actually called session:inquire.
// Standalone (no *Adapter receiver) so unit tests can drive it
// without bringing the session manager up.
// BuildInquiryReply is exported so the TUI adapter (and any future
// rich-client adapter) can reuse the same /approve|/deny|/respond
// parser. Behaviour matches the console inline renderer.
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
