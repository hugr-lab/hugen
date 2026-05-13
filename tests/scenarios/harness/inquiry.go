//go:build duckdb_arrow && scenario

package harness

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// InquiryRule is one scripted responder for a step's pending
// session:inquire calls. The harness pump intercepts every
// *protocol.InquiryRequest that bubbles to root and walks the
// active step's rules until it finds the first un-consumed
// match. Matched rules are consumed (one rule answers one
// inquiry); unmatched requests fall through to the runtime's
// timeout path on the originator. Phase 5.1 § κ.
type InquiryRule struct {
	Match   InquiryMatch  `json:"match" yaml:"match"`
	Respond InquiryAnswer `json:"respond" yaml:"respond"`
	Delay   Duration      `json:"delay,omitempty" yaml:"delay,omitempty"`
}

// InquiryMatch selects which incoming InquiryRequest a rule
// answers. Empty Type matches any type; QuestionContains is a
// case-insensitive substring check on Payload.Question. Both
// fields AND together when set.
type InquiryMatch struct {
	Type             string `json:"type,omitempty" yaml:"type,omitempty"`                           // approval | clarification
	QuestionContains string `json:"question_contains,omitempty" yaml:"question_contains,omitempty"` //nolint:tagliatelle
}

// InquiryAnswer is the scripted response a matched rule emits.
// Approved is a pointer so YAML `approved: false` differs from
// "field unset" — required for approval-type responses where
// `false` is the meaningful answer. When Timeout is true the
// harness skips Deliver entirely and lets the runtime inquire
// timeout fire on its own (useful for negative-path scenarios).
type InquiryAnswer struct {
	Approved *bool  `json:"approved,omitempty" yaml:"approved,omitempty"`
	Response string `json:"response,omitempty" yaml:"response,omitempty"`
	Reason   string `json:"reason,omitempty" yaml:"reason,omitempty"`
	Timeout  bool   `json:"timeout,omitempty" yaml:"timeout,omitempty"`
}

// inquiryDispatcher is the per-step responder pool. SessionHandle
// swaps a fresh instance in at the start of each Step() and clears
// the slot on return. Goroutine-safe — the harness pump dispatches
// from its own goroutine and a step may race late notifications.
type inquiryDispatcher struct {
	mu       sync.Mutex
	rules    []InquiryRule
	consumed []bool
}

func newInquiryDispatcher(rules []InquiryRule) *inquiryDispatcher {
	if len(rules) == 0 {
		return nil
	}
	return &inquiryDispatcher{
		rules:    rules,
		consumed: make([]bool, len(rules)),
	}
}

// match picks the first un-consumed rule whose Match accepts req,
// marks it consumed, and returns it. Returns nil when no rule
// matches — the caller logs and drops; the runtime times the
// inquire call out on its own.
func (d *inquiryDispatcher) match(req *protocol.InquiryRequest) *InquiryRule {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := range d.rules {
		if d.consumed[i] {
			continue
		}
		if !inquiryMatches(d.rules[i].Match, req) {
			continue
		}
		d.consumed[i] = true
		return &d.rules[i]
	}
	return nil
}

func inquiryMatches(m InquiryMatch, req *protocol.InquiryRequest) bool {
	if m.Type != "" && !strings.EqualFold(m.Type, req.Payload.Type) {
		return false
	}
	if m.QuestionContains != "" &&
		!strings.Contains(
			strings.ToLower(req.Payload.Question),
			strings.ToLower(m.QuestionContains),
		) {
		return false
	}
	return true
}

// buildInquiryResponse synthesises the InquiryResponse frame the
// harness Deliver()s to root. CallerSessionID + RequestID copy
// from the incoming request so dispatchInquiryResponse can cascade
// down via parent responseRouting tables. The frame's SessionID
// (BaseFrame) is rootID — that's the inbox Deliver targets;
// RouteInternal forwards from there. Phase 5.1 § 2.3.
func buildInquiryResponse(rootID string, author protocol.ParticipantInfo,
	req *protocol.InquiryRequest, ans InquiryAnswer,
) *protocol.InquiryResponse {
	payload := protocol.InquiryResponsePayload{
		RequestID:       req.Payload.RequestID,
		CallerSessionID: req.Payload.CallerSessionID,
		Reason:          ans.Reason,
		Response:        ans.Response,
		RespondedAt:     time.Now().UTC().Format(time.RFC3339Nano),
	}
	if ans.Approved != nil {
		v := *ans.Approved
		payload.Approved = &v
	}
	return protocol.NewInquiryResponse(rootID, author, payload)
}

// handleInquiry is the goroutine entry the pump spawns for each
// *protocol.InquiryRequest. Runs the matcher; on match, sleeps
// the optional Delay and Deliver()s the response to root. On a
// miss (or an explicit `timeout: true` rule) it just logs — the
// runtime's inquire tool times out on its own.
func (h *SessionHandle) handleInquiry(req protocol.InquiryRequest) {
	d := h.dispatcher.Load()
	if d == nil {
		h.t.Logf("inquiry %s type=%s ignored: no active responder",
			req.Payload.RequestID, req.Payload.Type)
		return
	}
	rule := d.match(&req)
	if rule == nil {
		h.t.Logf("inquiry %s type=%s q=%q ignored: no matching rule",
			req.Payload.RequestID, req.Payload.Type,
			singleLine(req.Payload.Question, 80))
		return
	}
	if rule.Respond.Timeout {
		h.t.Logf("inquiry %s type=%s matched timeout rule: skip Deliver",
			req.Payload.RequestID, req.Payload.Type)
		return
	}
	if delay := rule.Delay.Std(); delay > 0 {
		time.Sleep(delay)
	}
	author := protocol.ParticipantInfo{
		ID:   "harness-user",
		Kind: protocol.ParticipantUser,
		Name: "harness",
	}
	resp := buildInquiryResponse(h.id, author, &req, rule.Respond)
	deliverCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.rt.Core.Manager.Deliver(deliverCtx, h.id, resp); err != nil {
		h.t.Logf("inquiry %s Deliver: %v", req.Payload.RequestID, err)
		return
	}
	h.t.Logf("inquiry %s type=%s answered approved=%s response=%q",
		req.Payload.RequestID, req.Payload.Type,
		approvedLabel(rule.Respond.Approved),
		singleLine(rule.Respond.Response, 80))
}

func approvedLabel(b *bool) string {
	if b == nil {
		return "<unset>"
	}
	if *b {
		return "true"
	}
	return "false"
}
