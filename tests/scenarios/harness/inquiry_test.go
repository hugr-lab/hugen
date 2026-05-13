//go:build duckdb_arrow && scenario

package harness

import (
	"testing"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

func TestInquiryDispatcherMatchAndConsume(t *testing.T) {
	yes := true
	no := false
	d := newInquiryDispatcher([]InquiryRule{
		{
			Match:   InquiryMatch{Type: "clarification", QuestionContains: "revenue"},
			Respond: InquiryAnswer{Response: "by revenue"},
		},
		{
			Match:   InquiryMatch{Type: "clarification"},
			Respond: InquiryAnswer{Response: "fallback"},
		},
		{
			Match:   InquiryMatch{Type: "approval"},
			Respond: InquiryAnswer{Approved: &no, Reason: "denied"},
		},
		{
			Match:   InquiryMatch{Type: "approval"},
			Respond: InquiryAnswer{Approved: &yes},
		},
	})
	if d == nil {
		t.Fatalf("dispatcher should be non-nil for non-empty rules")
	}

	mk := func(typ, q string) *protocol.InquiryRequest {
		return &protocol.InquiryRequest{
			Payload: protocol.InquiryRequestPayload{
				Type:     typ,
				Question: q,
			},
		}
	}

	// First clarification: matches rule 0 (revenue), consumes it.
	got := d.match(mk("clarification", "by revenue or by volume?"))
	if got == nil || got.Respond.Response != "by revenue" {
		t.Fatalf("expected rule 0 match, got %+v", got)
	}

	// Second clarification: rule 0 consumed, falls through to rule 1.
	got = d.match(mk("clarification", "completely unrelated"))
	if got == nil || got.Respond.Response != "fallback" {
		t.Fatalf("expected rule 1 match, got %+v", got)
	}

	// First approval: rule 2.
	got = d.match(mk("approval", "delete /tmp/x?"))
	if got == nil || got.Respond.Approved == nil || *got.Respond.Approved != false {
		t.Fatalf("expected denied rule 2 match, got %+v", got)
	}

	// Second approval: rule 3.
	got = d.match(mk("approval", "delete /tmp/y?"))
	if got == nil || got.Respond.Approved == nil || *got.Respond.Approved != true {
		t.Fatalf("expected approved rule 3 match, got %+v", got)
	}

	// All consumed: subsequent call returns nil.
	if got := d.match(mk("approval", "another")); got != nil {
		t.Fatalf("expected nil after pool drained, got %+v", got)
	}
}

func TestInquiryDispatcherNoMatchReturnsNil(t *testing.T) {
	d := newInquiryDispatcher([]InquiryRule{
		{Match: InquiryMatch{Type: "approval"}, Respond: InquiryAnswer{}},
	})
	got := d.match(&protocol.InquiryRequest{
		Payload: protocol.InquiryRequestPayload{Type: "clarification"},
	})
	if got != nil {
		t.Fatalf("expected nil for type mismatch, got %+v", got)
	}
}

func TestInquiryDispatcherEmptyRulesNil(t *testing.T) {
	if d := newInquiryDispatcher(nil); d != nil {
		t.Fatalf("expected nil dispatcher for empty rules")
	}
	if d := newInquiryDispatcher([]InquiryRule{}); d != nil {
		t.Fatalf("expected nil dispatcher for empty rules")
	}
}

func TestInquiryMatchesCaseInsensitive(t *testing.T) {
	req := &protocol.InquiryRequest{
		Payload: protocol.InquiryRequestPayload{
			Type:     "Clarification",
			Question: "Should I use REVENUE or volume?",
		},
	}
	if !inquiryMatches(InquiryMatch{Type: "clarification", QuestionContains: "revenue"}, req) {
		t.Fatalf("case-insensitive type+question match should hit")
	}
	if !inquiryMatches(InquiryMatch{}, req) {
		t.Fatalf("empty match should accept anything")
	}
	if inquiryMatches(InquiryMatch{QuestionContains: "missing token"}, req) {
		t.Fatalf("substring miss should reject")
	}
}

func TestBuildInquiryResponseCopiesRouting(t *testing.T) {
	yes := true
	author := protocol.ParticipantInfo{ID: "harness-user", Kind: protocol.ParticipantUser}
	req := &protocol.InquiryRequest{
		Payload: protocol.InquiryRequestPayload{
			RequestID:       "rq-42",
			CallerSessionID: "worker-7",
			Type:            "approval",
			Question:        "write /tmp?",
		},
	}
	resp := buildInquiryResponse("root-1", author, req, InquiryAnswer{
		Approved: &yes,
		Reason:   "ok",
	})
	if resp.SessionID() != "root-1" {
		t.Fatalf("response SessionID should be rootID, got %s", resp.SessionID())
	}
	if resp.Payload.RequestID != "rq-42" {
		t.Fatalf("RequestID not copied: %s", resp.Payload.RequestID)
	}
	if resp.Payload.CallerSessionID != "worker-7" {
		t.Fatalf("CallerSessionID not copied: %s", resp.Payload.CallerSessionID)
	}
	if resp.Payload.Approved == nil || !*resp.Payload.Approved {
		t.Fatalf("Approved not set: %+v", resp.Payload.Approved)
	}
	if resp.Payload.RespondedAt == "" {
		t.Fatalf("RespondedAt not stamped")
	}
}
