package console

import (
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

func TestParseApprovalReply(t *testing.T) {
	cases := []struct {
		name         string
		in           string
		wantApproved bool
		wantReason   string
		wantErr      bool
	}{
		{"approve_slash", "/approve", true, "", false},
		{"approve_reason", "/approve looks ok", true, "looks ok", false},
		{"approve_yes_bare", "yes", true, "", false},
		{"approve_y", "y", true, "", false},
		{"approve_ok_with_reason", "ok safe", true, "safe", false},
		{"deny_slash", "/deny", false, "", false},
		{"deny_reason", "/deny not safe to run", false, "not safe to run", false},
		{"deny_no_bare", "no", false, "", false},
		{"deny_n", "n", false, "", false},
		{"empty_rejected", "", false, "", true},
		{"unknown_rejected", "maybe", false, "", true},
		{"slash_unknown_rejected", "/maybe", false, "", true},
		{"trim_whitespace", "  yes  ", true, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			approved, reason, err := parseApprovalReply(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got approved=%v reason=%q", approved, reason)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if approved != tc.wantApproved {
				t.Errorf("approved: got %v, want %v", approved, tc.wantApproved)
			}
			if reason != tc.wantReason {
				t.Errorf("reason: got %q, want %q", reason, tc.wantReason)
			}
		})
	}
}

func TestParseClarificationReply(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"bare_text", "by revenue", "by revenue", false},
		{"slash_respond", "/respond by revenue", "by revenue", false},
		{"slash_respond_trim", "  /respond   by revenue  ", "by revenue", false},
		{"empty_rejected", "", "", true},
		{"slash_respond_empty_rejected", "/respond", "", true},
		{"slash_respond_only_space_rejected", "/respond   ", "", true},
		{"multiword_kept_verbatim", `option "A"`, `option "A"`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseClarificationReply(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBuildInquiryReply_Approval verifies the constructed Frame
// carries the verdict, the original RequestID, and most
// importantly the original CallerSessionID — the field the
// cascade-down dispatcher matches on.
func TestBuildInquiryReply_Approval(t *testing.T) {
	user := protocol.ParticipantInfo{ID: "op", Kind: protocol.ParticipantUser, Name: "op"}
	pend := &PendingInquiry{
		RequestID:       "inq-abc",
		CallerSessionID: "worker-42",
		Kind:            protocol.InquiryTypeApproval,
	}
	resp, err := BuildInquiryReply(user, "root-1", pend, "/approve looks ok")
	if err != nil {
		t.Fatalf("BuildInquiryReply: %v", err)
	}
	if resp.Payload.RequestID != "inq-abc" {
		t.Errorf("RequestID: got %q, want inq-abc", resp.Payload.RequestID)
	}
	if resp.Payload.CallerSessionID != "worker-42" {
		t.Errorf("CallerSessionID: got %q, want worker-42", resp.Payload.CallerSessionID)
	}
	if resp.Payload.Approved == nil || !*resp.Payload.Approved {
		t.Errorf("Approved: want true, got %v", resp.Payload.Approved)
	}
	if resp.Payload.Reason != "looks ok" {
		t.Errorf("Reason: got %q, want %q", resp.Payload.Reason, "looks ok")
	}
	if resp.SessionID() != "root-1" {
		t.Errorf("SessionID: got %q, want root-1 (root inbox target)", resp.SessionID())
	}
}

func TestBuildInquiryReply_Clarification(t *testing.T) {
	user := protocol.ParticipantInfo{ID: "op", Kind: protocol.ParticipantUser, Name: "op"}
	pend := &PendingInquiry{
		RequestID:       "inq-xyz",
		CallerSessionID: "mission-7",
		Kind:            protocol.InquiryTypeClarification,
	}
	resp, err := BuildInquiryReply(user, "root-1", pend, "by revenue")
	if err != nil {
		t.Fatalf("BuildInquiryReply: %v", err)
	}
	if resp.Payload.Response != "by revenue" {
		t.Errorf("Response: got %q, want %q", resp.Payload.Response, "by revenue")
	}
	if resp.Payload.CallerSessionID != "mission-7" {
		t.Errorf("CallerSessionID: got %q, want mission-7", resp.Payload.CallerSessionID)
	}
	if resp.Payload.RespondedAt == "" {
		t.Errorf("RespondedAt: empty, want non-empty timestamp")
	}
	if !strings.Contains(resp.Payload.RespondedAt, "T") {
		t.Errorf("RespondedAt: got %q, want RFC3339Nano", resp.Payload.RespondedAt)
	}
}

func TestSplitFirstToken(t *testing.T) {
	cases := []struct {
		in, head, rest string
	}{
		{"", "", ""},
		{"approve", "approve", ""},
		{"approve safe", "approve", "safe"},
		{"approve  reason with  spaces", "approve", "reason with  spaces"},
		{"  yes  ", "yes", ""},
	}
	for _, tc := range cases {
		head, rest := splitFirstToken(tc.in)
		if head != tc.head || rest != tc.rest {
			t.Errorf("splitFirstToken(%q): got (%q, %q), want (%q, %q)",
				tc.in, head, rest, tc.head, tc.rest)
		}
	}
}
