package task

import (
	"context"
	"fmt"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/skill"
)

// taskAutoApproveToolsKey marks a task-worker session whose launch was
// approved "with tools" (§4.6 / §5.1). RunRecipe stamps it on the
// spawned child; [Extension.MaybeAutoApprove] walks the caller chain to
// it and grants the worker's tool calls without re-prompting. The value
// is a bool — true means auto-approve.
//
// This is the task ext's OWN auto-approve channel, independent of
// MissionState: a task launched from a root chat has no mission
// ancestor, so the mission ext's MaybeAutoApprove can't cover it. The
// nested-mission-worker case stays on the mission channel (its
// MaybeAutoApprove already walks to the granting mission).
const taskAutoApproveToolsKey = "task.launch.auto_approve_tools"

// launchDecision is the interpreted result of a §5.1 launch-approval
// modal: whether to run the task at all, and whether its internal tools
// auto-approve.
type launchDecision struct {
	approved         bool
	autoApproveTools bool
	refine           string
}

// raiseLaunchApproval shows the §5.1 launch modal on the anchor session
// and interprets the response. A task is STANDARDIZED — the user
// approves LAUNCHING it (with or without auto-approving its tools), not
// each internal tool. Reuses the §4.6 approval modal (Type=approval +
// AutoApproveTools), so no new adapter surface is needed.
//
// Raised from the tool-dispatch goroutine on the anchor (root) session;
// the session's main loop routes the InquiryResponse back to the
// blocked call — the same concurrency model the model-driven
// session:inquire tool relies on.
func (e *Extension) raiseLaunchApproval(ctx context.Context, anchor *session.Session, recipe string, sk skill.Skill) (launchDecision, error) {
	goal := strings.TrimSpace(sk.Manifest.Hugen.Task.GoalSummary)
	if goal == "" {
		goal = strings.TrimSpace(sk.Manifest.Description)
	}
	resp, err := anchor.RequestInquiry(ctx, protocol.InquiryRequestPayload{
		Type:     protocol.InquiryTypeApproval,
		Question: fmt.Sprintf("Run task %q?", recipe),
		Context:  launchApprovalContext(goal),
	})
	if err != nil {
		return launchDecision{}, fmt.Errorf("launch approval inquiry: %w", err)
	}
	if resp == nil {
		return launchDecision{}, fmt.Errorf("launch approval inquiry: nil response")
	}
	return interpretLaunchResponse(resp), nil
}

// interpretLaunchResponse maps an approval InquiryResponse onto a
// launchDecision. approve-with-tools ⇒ approved + autoApproveTools;
// approve ⇒ approved only; reject (Approved=false) or refine
// (Approved=nil) ⇒ not approved, with any free-text carried as refine.
func interpretLaunchResponse(resp *protocol.InquiryResponse) launchDecision {
	if resp == nil {
		return launchDecision{}
	}
	approved := resp.Payload.Approved != nil && *resp.Payload.Approved
	return launchDecision{
		approved:         approved,
		autoApproveTools: approved && resp.Payload.AutoApproveTools,
		refine:           strings.TrimSpace(resp.Payload.Response),
	}
}

// launchApprovalContext renders the modal body: the task's goal plus a
// one-line explanation of the two approve modes so the §4.6 choices read
// sensibly for a task launch (vs. a plan approval).
func launchApprovalContext(goal string) string {
	const modes = "“Approve with tools” auto-approves this task's tools for the run; “Approve” runs it with each tool still prompting."
	if goal == "" {
		return modes
	}
	return goal + "\n\n" + modes
}

// MaybeAutoApprove implements [extension.ToolApprovalPolicy] for the
// task ext. It grants any tool call from a task-worker (or a descendant)
// whose launch was approved "with tools" — walking the caller chain to
// the [taskAutoApproveToolsKey] stamp RunRecipe set at spawn. A
// standardized task's internal tools then don't re-prompt one-by-one.
//
// Returns ("", false) when no ancestor carries the stamp — the runtime
// falls through to the next policy / the normal approval inquiry. The
// worker can only call tools already in its allow-set (FilterTools), so
// a blanket grant equals "auto-approve the task's allowed_tools_default"
// (§5.1) without re-checking the tool name.
func (e *Extension) MaybeAutoApprove(_ context.Context, caller extension.SessionState, _ string) (string, bool) {
	for s := caller; s != nil; {
		if v, ok := s.Value(taskAutoApproveToolsKey); ok {
			if b, _ := v.(bool); b {
				return s.SessionID(), true
			}
		}
		parent, ok := s.Parent()
		if !ok || parent == nil {
			return "", false
		}
		s = parent
	}
	return "", false
}
