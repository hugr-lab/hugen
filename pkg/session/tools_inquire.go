package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// session:inquire — unified HITL primitive. One tool, two
// types: approval (yes/no with optional reason) and clarification
// (free-form text with optional options list). Blocks the calling
// session until the adapter delivers an InquiryResponse, the
// per-call timeout fires, or the call ctx cancels. Phase 5.1 § 2.

const inquireSchema = `{
  "type": "object",
  "properties": {
    "type":       {"type": "string", "enum": ["approval", "clarification"], "description": "REQUIRED. Must be exactly \"approval\" (yes/no answer) or \"clarification\" (free-form text). Calls without this field are rejected."},
    "question":   {"type": "string", "description": "REQUIRED. Concise stand-alone question shown to the user — they may not see the rest of the agent's reasoning."},
    "context":    {"type": "string", "description": "Optional extra context the user might need to answer. Keep short — long context belongs in the question."},
    "options":    {"type": "array", "items": {"type": "string"}, "description": "Optional pre-defined answers for clarification questions."},
    "timeout_ms": {"type": "integer", "minimum": 1, "description": "Per-call deadline override. Defaults to the runtime-configured global timeout."}
  },
  "required": ["type", "question"],
  "examples": [
    {"type": "clarification", "question": "Which data source should I use?", "context": "A [fits-explicit], B [fits-possibly]", "options": ["A", "B"]},
    {"type": "approval", "question": "Run DROP TABLE on staging.users?"}
  ]
}`

type inquireInput struct {
	Type      string   `json:"type"`
	Question  string   `json:"question"`
	Context   string   `json:"context,omitempty"`
	Options   []string `json:"options,omitempty"`
	TimeoutMs int      `json:"timeout_ms,omitempty"`
}

type approvalResult struct {
	Approved      bool   `json:"approved"`
	Reason        string `json:"reason,omitempty"`
	Timeout       bool   `json:"timeout,omitempty"`
	DefaultAction string `json:"default_action,omitempty"`
}

type clarificationResult struct {
	Response    string `json:"response,omitempty"`
	RespondedAt string `json:"responded_at,omitempty"`
	Timeout     bool   `json:"timeout,omitempty"`
}

// defaultInquireTimeoutMs is the runtime fallback when the
// caller omits timeout_ms and Deps.DefaultInquireTimeoutMs is
// unset. One hour is the spec's suggested production default
// (§ 2.7); a tighter value keeps tests responsive.
const defaultInquireTimeoutMs = 60 * 60 * 1000

func (s *Session) callInquire(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if s.IsClosed() {
		return toolErr("session_gone", "calling session has already terminated")
	}
	var in inquireInput
	if err := json.Unmarshal(args, &in); err != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid inquire args: %v", err))
	}
	switch in.Type {
	case protocol.InquiryTypeApproval, protocol.InquiryTypeClarification:
	default:
		return toolErr("bad_request",
			fmt.Sprintf("type must be approval|clarification; got %q", in.Type))
	}
	if strings.TrimSpace(in.Question) == "" {
		return toolErr("bad_request", "question is required")
	}
	timeoutMs := in.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = s.resolveInquireTimeout()
	}
	requestID := newInquiryRequestID()

	// Register the pending channel BEFORE emitting the request so
	// a fast cascade-down (test fixtures with synchronous adapter
	// loops) cannot deliver before the entry exists.
	respCh := s.recordPending(requestID)
	defer s.clearPending(requestID)

	// Register the active tool feed so an InquiryResponse arriving
	// at the caller's hop matches by RequestID and delivers to
	// our pending channel. The dispatcher's deliverPending writes
	// to the same channel — both paths converge on the buffered
	// chan of 1.
	blockingState := protocol.SessionStatusWaitUserInput
	if in.Type == protocol.InquiryTypeApproval {
		blockingState = protocol.SessionStatusWaitApproval
	}
	feed := &ToolFeed{
		Consumes: func(f protocol.Frame) bool {
			resp, ok := f.(*protocol.InquiryResponse)
			if !ok {
				return false
			}
			return resp.Payload.RequestID == requestID
		},
		Feed: func(f protocol.Frame) {
			resp, ok := f.(*protocol.InquiryResponse)
			if !ok {
				return
			}
			// The internal dispatcher already pushed via
			// deliverPending; this Feed callback is the
			// secondary path for responses arriving through
			// RouteToolFeed (e.g. a fixture submitting directly
			// without round-tripping the internal handler). The
			// channel is buffered to 1 — duplicate landings drop
			// silently.
			select {
			case respCh <- resp:
			default:
			}
		},
		BlockingState:  blockingState,
		BlockingReason: "tool=inquire type=" + in.Type,
	}
	release := s.registerToolFeed(ctx, feed)
	defer release()

	// Emit the InquiryRequest on the caller's outbox. emit
	// persists in session_events AND fans out to subscribers of
	// the caller's session id AND lands in any parent pump that
	// is reading this session's outbox — exactly the chain the
	// bubble case in subagent_pump.go projects.
	req := protocol.NewInquiryRequest(s.id, s.agent.Participant(),
		protocol.InquiryRequestPayload{
			RequestID:       requestID,
			CallerSessionID: s.id,
			Type:            in.Type,
			Question:        in.Question,
			Context:         in.Context,
			Options:         in.Options,
			TimeoutMs:       timeoutMs,
		})
	if err := s.emit(ctx, req); err != nil {
		return toolErr("io", fmt.Sprintf("inquire: emit request: %v", err))
	}

	deadline := time.NewTimer(time.Duration(timeoutMs) * time.Millisecond)
	defer deadline.Stop()

	select {
	case resp := <-respCh:
		return marshalInquiryResponse(in.Type, resp)
	case <-deadline.C:
		return marshalInquiryTimeout(s, in.Type)
	case <-ctx.Done():
		return toolErr("cancelled",
			fmt.Sprintf("inquire aborted: %v", ctx.Err()))
	}
}

// resolveInquireTimeout returns the per-call deadline in ms.
// Deps.DefaultInquireTimeoutMs takes precedence when non-zero;
// otherwise the package-level fallback applies. Operators wire
// the deps value from config.yaml.hitl.default_timeout_ms.
func (s *Session) resolveInquireTimeout() int {
	if s.deps != nil && s.deps.DefaultInquireTimeoutMs > 0 {
		return s.deps.DefaultInquireTimeoutMs
	}
	return defaultInquireTimeoutMs
}

func marshalInquiryResponse(kind string, resp *protocol.InquiryResponse) (json.RawMessage, error) {
	if resp.Payload.Timeout {
		return marshalInquiryTimeoutPayload(kind, resp.Payload.Reason)
	}
	switch kind {
	case protocol.InquiryTypeApproval:
		approved := false
		if resp.Payload.Approved != nil {
			approved = *resp.Payload.Approved
		}
		return json.Marshal(approvalResult{
			Approved: approved,
			Reason:   resp.Payload.Reason,
		})
	case protocol.InquiryTypeClarification:
		return json.Marshal(clarificationResult{
			Response:    resp.Payload.Response,
			RespondedAt: resp.Payload.RespondedAt,
		})
	}
	// Defensive — schema validation already gated this.
	return json.Marshal(map[string]any{
		"response": resp.Payload.Response,
	})
}

func marshalInquiryTimeout(s *Session, kind string) (json.RawMessage, error) {
	// Render the timeout notice via the bundled template so
	// operators / users see consistent wording. Reason left blank;
	// the timeout fact itself is the diagnostic.
	notice := ""
	if s.deps != nil && s.deps.Prompts != nil {
		notice = strings.TrimRight(s.deps.Prompts.MustRender(
			"inquiry/timeout_notice",
			map[string]any{
				"Timeout":       "configured",
				"DefaultAction": "deny",
			},
		), "\n")
	}
	return marshalInquiryTimeoutPayload(kind, notice)
}

func marshalInquiryTimeoutPayload(kind, reason string) (json.RawMessage, error) {
	switch kind {
	case protocol.InquiryTypeApproval:
		return json.Marshal(approvalResult{
			Approved:      false,
			Timeout:       true,
			DefaultAction: "deny",
			Reason:        reason,
		})
	case protocol.InquiryTypeClarification:
		return json.Marshal(clarificationResult{
			Timeout: true,
		})
	}
	return json.Marshal(map[string]any{"timeout": true, "reason": reason})
}

// newInquiryRequestID is a 32-char hex random id sized to match
// the existing newFrameID convention in pkg/protocol.
func newInquiryRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "inq-" + hex.EncodeToString(b[:])
}

// requestApproval is the runtime-initiated approval helper the
// dispatcher uses for RequiresApproval-tagged tools. Phase 5.1
// § 2.6: build an approval payload from tool name + args summary,
// run the same internal inquire flow as the user-callable tool,
// return (approved, reason, err). On timeout: approved=false
// with reason carrying the timeout notice. On ctx cancel: err
// propagates so the dispatcher surfaces an io error.
func (s *Session) requestApproval(ctx context.Context, toolName, argsSummary string) (bool, string, error) {
	question := ""
	if s.deps != nil && s.deps.Prompts != nil {
		question = strings.TrimRight(s.deps.Prompts.MustRender(
			"inquiry/approval_request_summary",
			map[string]any{
				"ToolName":    toolName,
				"ArgsSummary": argsSummary,
			},
		), "\n")
	} else {
		question = fmt.Sprintf("Run %s with args: %s ?", toolName, argsSummary)
	}
	args, _ := json.Marshal(inquireInput{
		Type:     protocol.InquiryTypeApproval,
		Question: question,
	})
	raw, err := s.callInquire(ctx, args)
	if err != nil {
		return false, "", err
	}
	var res approvalResult
	if uerr := json.Unmarshal(raw, &res); uerr != nil {
		return false, "", fmt.Errorf("approval: parse response: %w", uerr)
	}
	if res.Timeout {
		return false, res.Reason, nil
	}
	return res.Approved, res.Reason, nil
}
