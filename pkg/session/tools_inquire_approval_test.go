package session

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// stubAutoApprovePolicy is the minimal extension that implements
// [extension.ToolApprovalPolicy]. It records every (caller, tool)
// pair it sees and grants based on a configured predicate.
type stubAutoApprovePolicy struct {
	name      string
	grant     bool
	mission   string
	mu        sync.Mutex
	calls     []autoApproveCall
	callCount atomic.Int32
}

type autoApproveCall struct {
	CallerID string
	Tool     string
}

func (s *stubAutoApprovePolicy) Name() string { return s.name }

func (s *stubAutoApprovePolicy) MaybeAutoApprove(_ context.Context, caller extension.SessionState, tool string) (string, bool) {
	s.callCount.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	id := ""
	if caller != nil {
		id = caller.SessionID()
	}
	s.calls = append(s.calls, autoApproveCall{CallerID: id, Tool: tool})
	if s.grant {
		return s.mission, true
	}
	return "", false
}

// TestRequestApproval_AutoApprovePolicy_GrantsWithoutModal verifies
// the §4.6.5 wiring: when an ext implementing ToolApprovalPolicy
// returns (id, true) for the caller, requestApproval short-circuits
// — no callInquire fires, no user modal opens, the caller sees
// (approved=true, "", nil) instantly.
//
// This is the load-bearing integration test for the policy hook:
// without it the runtime would still open the user modal on every
// tool call even when the mission already granted blanket
// approval at plan-acceptance time.
func TestRequestApproval_AutoApprovePolicy_GrantsWithoutModal(t *testing.T) {
	policy := &stubAutoApprovePolicy{
		name:    "stub-policy",
		grant:   true,
		mission: "mis-granted",
	}
	parent, cleanup := newTestParent(t, withTestExtensions(policy))
	defer cleanup()

	approved, reason, err := parent.requestApproval(context.Background(), "bash-mcp:run", `{"cmd":"echo hi"}`)
	if err != nil {
		t.Fatalf("requestApproval: %v", err)
	}
	if !approved {
		t.Errorf("approved = false; want true (policy hook should have granted)")
	}
	if reason != "" {
		t.Errorf("reason = %q; want empty on auto-grant", reason)
	}
	if got := policy.callCount.Load(); got != 1 {
		t.Errorf("policy hook called %d times; want exactly 1", got)
	}
	policy.mu.Lock()
	defer policy.mu.Unlock()
	if len(policy.calls) != 1 || policy.calls[0].Tool != "bash-mcp:run" {
		t.Errorf("policy received unexpected calls: %+v", policy.calls)
	}
}

// TestRequestApproval_AutoApprovePolicy_FirstHitWins verifies the
// dispatch order: when multiple extensions implement the policy
// interface, the first one returning ok=true is the winner and the
// rest are not consulted. Matters because deps.Extensions is
// ordered (registration order); whichever extension is wired first
// gets first say.
func TestRequestApproval_AutoApprovePolicy_FirstHitWins(t *testing.T) {
	first := &stubAutoApprovePolicy{
		name:    "first",
		grant:   true,
		mission: "mis-first",
	}
	second := &stubAutoApprovePolicy{
		name:  "second",
		grant: true, // would also grant — but should never be asked.
	}
	parent, cleanup := newTestParent(t, withTestExtensions(first, second))
	defer cleanup()

	approved, _, err := parent.requestApproval(context.Background(), "tool:x", "{}")
	if err != nil {
		t.Fatalf("requestApproval: %v", err)
	}
	if !approved {
		t.Errorf("approved = false; first policy should have granted")
	}
	if got := first.callCount.Load(); got != 1 {
		t.Errorf("first policy callCount = %d; want 1", got)
	}
	if got := second.callCount.Load(); got != 0 {
		t.Errorf("second policy callCount = %d; want 0 (first hit wins)", got)
	}
}

// stubMissionPolicyName is the marker the policy registers under —
// kept distinct from "mission" so this test doesn't collide with
// any production extension that might also implement the interface.
const stubMissionPolicyName = "stub-policy"

// ensure stubAutoApprovePolicy implements the interface.
var _ extension.ToolApprovalPolicy = (*stubAutoApprovePolicy)(nil)

// stubDenyInquiryPolicy is the minimal extension implementing
// [extension.InquiryPolicy]. It denies (or not) per its flag and
// records how many times it was consulted.
type stubDenyInquiryPolicy struct {
	name      string
	deny      bool
	reason    string
	callCount atomic.Int32
}

func (s *stubDenyInquiryPolicy) Name() string { return s.name }

func (s *stubDenyInquiryPolicy) MaybeDenyInquiry(_ context.Context, _ extension.SessionState) (string, bool) {
	s.callCount.Add(1)
	if s.deny {
		return s.reason, true
	}
	return "", false
}

var _ extension.InquiryPolicy = (*stubDenyInquiryPolicy)(nil)

// decodeToolErrCode pulls the structured tool-error code out of a
// callInquire response. Returns "" when the body is not an error
// envelope (e.g. a normal/timeout inquiry result).
func decodeToolErrCode(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode tool response: %v\nraw: %s", err, raw)
	}
	return resp.Error.Code
}

// TestCallInquire_InquiryPolicyDenies verifies the Phase 6.2a gate:
// a registered InquiryPolicy that denies short-circuits callInquire
// into a structured denied_no_operator error before any park/bubble.
func TestCallInquire_InquiryPolicyDenies(t *testing.T) {
	policy := &stubDenyInquiryPolicy{name: "deny", deny: true, reason: "no operator at cron fire"}
	parent, cleanup := newTestParent(t, withTestExtensions(policy))
	defer cleanup()

	args, _ := json.Marshal(inquireInput{
		Type:     protocol.InquiryTypeClarification,
		Question: "which table did you mean?",
	})
	raw, err := parent.callInquire(context.Background(), args)
	if err != nil {
		t.Fatalf("callInquire: %v", err)
	}
	if code := decodeToolErrCode(t, raw); code != "denied_no_operator" {
		t.Fatalf("error code = %q, want denied_no_operator (raw: %s)", code, raw)
	}
	if got := policy.callCount.Load(); got != 1 {
		t.Errorf("policy consulted %d times, want 1", got)
	}
}

// TestCallInquire_InquiryPolicyAllows_FallsThrough verifies a
// non-denying policy leaves the normal park path intact: the inquiry
// is NOT denied, it parks and (with a 1ms deadline) returns a timeout
// rather than denied_no_operator.
func TestCallInquire_InquiryPolicyAllows_FallsThrough(t *testing.T) {
	policy := &stubDenyInquiryPolicy{name: "allow", deny: false}
	parent, cleanup := newTestParent(t, withTestExtensions(policy))
	defer cleanup()

	args, _ := json.Marshal(inquireInput{
		Type:      protocol.InquiryTypeClarification,
		Question:  "which table did you mean?",
		TimeoutMs: 1,
	})
	raw, err := parent.callInquire(context.Background(), args)
	if err != nil {
		t.Fatalf("callInquire: %v", err)
	}
	if code := decodeToolErrCode(t, raw); code == "denied_no_operator" {
		t.Fatalf("non-denying policy must fall through, got denied_no_operator (raw: %s)", raw)
	}
	if got := policy.callCount.Load(); got != 1 {
		t.Errorf("policy consulted %d times, want 1", got)
	}
}

// TestRequestApproval_InquiryDeny_BackstopsApproval verifies the
// combined path: a tool NOT auto-approved by any ToolApprovalPolicy
// reaches callInquire, where the InquiryPolicy deny turns the
// approval into a (not-approved) result instead of parking — the
// headless-cron backstop for requires_approval tools.
func TestRequestApproval_InquiryDeny_BackstopsApproval(t *testing.T) {
	policy := &stubDenyInquiryPolicy{name: "deny", deny: true, reason: "no operator at cron fire"}
	parent, cleanup := newTestParent(t, withTestExtensions(policy))
	defer cleanup()

	approved, _, err := parent.requestApproval(context.Background(), "bash-mcp:run", `{"cmd":"echo hi"}`)
	if err != nil {
		t.Fatalf("requestApproval: %v", err)
	}
	if approved {
		t.Errorf("approved = true; a denied inquiry must not approve the tool")
	}
	if got := policy.callCount.Load(); got != 1 {
		t.Errorf("policy consulted %d times, want 1", got)
	}
}
