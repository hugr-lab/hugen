package session

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/plan"
)

// init registers the four US2 plan tools into the package-level
// dispatch table. Per phase-4-spec §6 + contracts/tools-plan.md
// these surface as session:plan_set / session:plan_comment /
// session:plan_show / session:plan_clear. Permission objects per
// contracts/permission-objects.md.
//
// The four entries land in a separate file from manager_tools.go
// so the US1 sub-agent surface and the US2 plan surface stay
// independently reviewable and the dispatch table grows by file
// rather than by editing one block.
func init() {
	sessionTools["plan_set"] = sessionToolDescriptor{
		Name:             "plan_set",
		Description:      "Write or replace the plan body. Wipes the in-memory comment log; events are not deleted.",
		PermissionObject: permObjectPlanWrite,
		ArgSchema:        json.RawMessage(planSetSchema),
		Handler:          callPlanSet,
	}
	sessionTools["plan_comment"] = sessionToolDescriptor{
		Name:             "plan_comment",
		Description:      "Append a progress comment. Optionally moves the current-step pointer.",
		PermissionObject: permObjectPlanWrite,
		ArgSchema:        json.RawMessage(planCommentSchema),
		Handler:          callPlanComment,
	}
	sessionTools["plan_show"] = sessionToolDescriptor{
		Name:             "plan_show",
		Description:      "Return the full plan state — body + pointer + every retained comment since the last set.",
		PermissionObject: permObjectPlanRead,
		ArgSchema:        json.RawMessage(planShowSchema),
		Handler:          callPlanShow,
	}
	sessionTools["plan_clear"] = sessionToolDescriptor{
		Name:             "plan_clear",
		Description:      "Drop the plan entirely. Body and pointer no longer render in the system prompt.",
		PermissionObject: permObjectPlanWrite,
		ArgSchema:        json.RawMessage(planClearSchema),
		Handler:          callPlanClear,
	}
}

// Permission objects per contracts/permission-objects.md §"Plan
// system tools". The `set` / `comment` / `clear` ops share the
// write capability; `show` is gated by read.
const (
	permObjectPlanWrite = "hugen:plan:write"
	permObjectPlanRead  = "hugen:plan:read"
)

// JSON schemas. Kept verbatim with contracts/tools-plan.md so the
// LLM provider passes them through unchanged.
const (
	planSetSchema = `{
  "type": "object",
  "properties": {
    "text":         {"type": "string", "description": "Plan body. Capped at 8 KB after truncation marker."},
    "current_step": {"type": "string", "description": "Short pointer to the active step. Optional; preserves prior value when omitted."}
  },
  "required": ["text"]
}`

	planCommentSchema = `{
  "type": "object",
  "properties": {
    "text":         {"type": "string", "description": "Comment body. Capped at 2 KB after truncation marker."},
    "current_step": {"type": "string", "description": "Short pointer; optional, preserves prior value when omitted."}
  },
  "required": ["text"]
}`

	planShowSchema = `{
  "type": "object",
  "properties": {}
}`

	planClearSchema = `{
  "type": "object",
  "properties": {}
}`
)

// ---------- plan_set ----------

type planSetInput struct {
	Text        string `json:"text"`
	CurrentStep string `json:"current_step,omitempty"`
}

type planOKOutput struct {
	OK bool `json:"ok"`
}

func callPlanSet(ctx context.Context, _ *Manager, args json.RawMessage) (json.RawMessage, error) {
	caller, errFrame, err := callerSession(ctx)
	if errFrame != nil || err != nil {
		return errFrame, err
	}
	var in planSetInput
	if err := json.Unmarshal(args, &in); err != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid plan_set args: %v", err))
	}
	if in.Text == "" {
		return toolErr("bad_request", "text is required")
	}
	return persistAndApplyPlanOp(ctx, caller, plan.OpSet, in.Text, in.CurrentStep, true)
}

// ---------- plan_comment ----------

type planCommentInput struct {
	Text        string `json:"text"`
	CurrentStep string `json:"current_step,omitempty"`
}

func callPlanComment(ctx context.Context, _ *Manager, args json.RawMessage) (json.RawMessage, error) {
	caller, errFrame, err := callerSession(ctx)
	if errFrame != nil || err != nil {
		return errFrame, err
	}
	var in planCommentInput
	if err := json.Unmarshal(args, &in); err != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid plan_comment args: %v", err))
	}
	if in.Text == "" {
		return toolErr("bad_request", "text is required")
	}
	// Refuse a comment with no plan to attach to. Per
	// contracts/tools-plan.md §plan_comment we surface no_active_plan
	// rather than silently buffering — the model should call plan_set
	// first.
	caller.planMu.Lock()
	active := caller.plan.Active
	caller.planMu.Unlock()
	if !active {
		return toolErr("no_active_plan",
			"no plan_set precedes this comment; call plan_set first")
	}
	return persistAndApplyPlanOp(ctx, caller, plan.OpComment, in.Text, in.CurrentStep, true)
}

// ---------- plan_show ----------

type planShowOutput struct {
	Active      bool                  `json:"active"`
	Text        string                `json:"text,omitempty"`
	CurrentStep string                `json:"current_step,omitempty"`
	SetAt       string                `json:"set_at,omitempty"`
	UpdatedAt   string                `json:"updated_at,omitempty"`
	Comments    []planShowCommentRow  `json:"comments,omitempty"`
}

type planShowCommentRow struct {
	At          string `json:"at"`
	CurrentStep string `json:"current_step,omitempty"`
	Text        string `json:"text"`
}

func callPlanShow(ctx context.Context, _ *Manager, _ json.RawMessage) (json.RawMessage, error) {
	caller, errFrame, err := callerSession(ctx)
	if errFrame != nil || err != nil {
		return errFrame, err
	}
	caller.planMu.Lock()
	p := caller.plan
	caller.planMu.Unlock()
	if !p.Active {
		return json.Marshal(planShowOutput{Active: false})
	}
	out := planShowOutput{
		Active:      true,
		Text:        p.Text,
		CurrentStep: p.CurrentStep,
		SetAt:       p.SetAt.UTC().Format(time.RFC3339),
		UpdatedAt:   p.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if len(p.Comments) > 0 {
		out.Comments = make([]planShowCommentRow, 0, len(p.Comments))
		for _, c := range p.Comments {
			out.Comments = append(out.Comments, planShowCommentRow{
				At:          c.At.UTC().Format(time.RFC3339),
				CurrentStep: c.CurrentStep,
				Text:        c.Text,
			})
		}
	}
	return json.Marshal(out)
}

// ---------- plan_clear ----------

func callPlanClear(ctx context.Context, _ *Manager, _ json.RawMessage) (json.RawMessage, error) {
	caller, errFrame, err := callerSession(ctx)
	if errFrame != nil || err != nil {
		return errFrame, err
	}
	return persistAndApplyPlanOp(ctx, caller, plan.OpClear, "", "", false)
}

// persistAndApplyPlanOp is the shared write path for set / comment
// / clear handlers. It:
//
//  1. Resolves the effective current_step (preserving prior pointer
//     on omission per the contracts) under planMu so two concurrent
//     handlers can't read the same prior value.
//  2. Emits a PlanOp Frame via s.emit — store write + outbox push.
//  3. On a successful persist, applies the op to the in-memory
//     projection so the next systemPrompt render sees the new state
//     without a full Project rebuild.
//
// preservePriorStep=true → set / comment use prior pointer when the
// caller omits current_step. Clear ignores the pointer entirely.
//
// Holding planMu across emit serialises plan tool calls within a
// session — acceptable because plan ops are rare. If emit fails the
// in-memory mirror stays untouched, mirroring "events are the source
// of truth" — the next materialise will rebuild from whatever did
// land.
func persistAndApplyPlanOp(ctx context.Context, s *Session, op, text, currentStep string, preservePriorStep bool) (json.RawMessage, error) {
	s.planMu.Lock()
	defer s.planMu.Unlock()

	if preservePriorStep && currentStep == "" {
		currentStep = s.plan.CurrentStep
	}

	frame := protocol.NewPlanOp(s.id, s.agent.Participant(), protocol.PlanOpPayload{
		Op:          op,
		Text:        text,
		CurrentStep: currentStep,
	})
	if err := s.emit(ctx, frame); err != nil {
		return toolErr("io", fmt.Sprintf("emit plan_op: %v", err))
	}
	s.plan = plan.Apply(s.plan, plan.ProjectEvent{
		At:          frame.OccurredAt(),
		Op:          op,
		Text:        text,
		CurrentStep: currentStep,
	})
	return json.Marshal(planOKOutput{OK: true})
}
