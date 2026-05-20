package mission

import (
	"context"
	"fmt"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// runApprovalInquire is the runtime-side approval gate (Phase I,
// design 003). After the planner closes a valid plan handoff, the
// mission's auto-runner calls this to surface the plan to the
// user as a session:inquire(type=approval) — rendered from the
// typed Plan body, not from prose the model produced. The user
// answers; on approve the loop proceeds to the wave, on deny the
// caller folds the response into a synthetic verdict-amend and
// replans on the next iteration.
//
// Returns (approved, reason, error). `reason` carries the user's
// optional decline text (or the canned options answer for
// clarification-style replies) so the planner sees what to
// address. `error` is reserved for infrastructure failures
// (timeout, ctx cancel, render fault) — the caller treats those
// as mission-abort.
func (e *Extension) runApprovalInquire(ctx context.Context, mission extension.SessionState, plan Plan) (bool, string, error) {
	question, err := renderApprovalQuestion(mission, plan)
	if err != nil {
		return false, "", fmt.Errorf("mission: approval: render question: %w", err)
	}
	payload := protocol.InquiryRequestPayload{
		Type:     protocol.InquiryTypeApproval,
		Question: question,
		Context:  plan.Rationale,
	}
	resp, err := mission.RequestInquiry(ctx, payload)
	if err != nil {
		return false, "", err
	}
	if resp == nil {
		return false, "", fmt.Errorf("mission: approval: nil response")
	}
	if resp.Payload.Timeout {
		return false, "approval inquire timed out", nil
	}
	if resp.Payload.Approved != nil {
		if *resp.Payload.Approved {
			return true, "", nil
		}
		reason := strings.TrimSpace(resp.Payload.Reason)
		if reason == "" {
			reason = "user denied approval without a reason"
		}
		return false, reason, nil
	}
	// Adapter responded with free-text (clarification path) rather
	// than an explicit approve/deny — treat the response body as
	// amend feedback. Empty text degrades to a generic decline so
	// the planner replans rather than running an unconfirmed plan.
	free := strings.TrimSpace(resp.Payload.Response)
	if free == "" {
		return false, "user reply was empty — treating as decline", nil
	}
	return false, "user reply: " + free, nil
}

// approvalQuestionView is the typed payload the
// `mission/approval_question` template renders against. Mirrors
// the planner's mission frame (goal + AC) + NextWave + Roadmap +
// Rationale so the user sees the same contract the runtime is
// about to hash into the plan marker. The mission frame is what
// the user is actually signing off on — wave/roadmap can evolve
// freely after approval, but mission_goal + mission_acceptance_
// criteria changes reopen the modal.
type approvalQuestionView struct {
	MissionGoal               string
	MissionAcceptanceCriteria []string
	NextWave                  approvalWaveView
	WaveAcceptanceCriteria    []string
	Roadmap                   []RoadmapEntry
	Rationale                 string
}

type approvalWaveView struct {
	Label     string
	Subagents []approvalSubagentView
}

type approvalSubagentView struct {
	Name string
	Role string
	Task string
}

// renderApprovalQuestion projects the typed plan body into the
// view shape and renders the bundled template. Long worker tasks
// are truncated to keep the inquire modal readable — the user
// reads a high-level pitch, not the full per-worker brief.
func renderApprovalQuestion(mission extension.SessionState, plan Plan) (string, error) {
	view := approvalQuestionView{
		MissionGoal:               strings.TrimSpace(plan.MissionGoal),
		MissionAcceptanceCriteria: trimStrings(plan.MissionAcceptanceCriteria),
		NextWave: approvalWaveView{
			Label:     plan.NextWave.Label,
			Subagents: make([]approvalSubagentView, 0, len(plan.NextWave.Subagents)),
		},
		WaveAcceptanceCriteria: trimStrings(plan.NextWave.AcceptanceCriteria),
		Roadmap:                plan.Roadmap,
		Rationale:              strings.TrimSpace(plan.Rationale),
	}
	for _, s := range plan.NextWave.Subagents {
		view.NextWave.Subagents = append(view.NextWave.Subagents, approvalSubagentView{
			Name: s.Name,
			Role: s.Role,
			Task: shortenForInquire(s.Task),
		})
	}
	renderer := mission.Prompts()
	if renderer == nil {
		return "", fmt.Errorf("mission: approval: no prompts renderer on session")
	}
	return renderer.Render("mission/approval_question", view)
}

// trimStrings returns a copy with each entry TrimSpaced and empty
// entries dropped. Keeps the rendered template tight — empty bullets
// in a modal look like a planner bug.
func trimStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

// shortenForInquire trims a long worker-task brief down to a
// single user-friendly sentence — the inquire modal is not the
// place to drop the full plan-time brief. Empty / short inputs
// pass through verbatim.
func shortenForInquire(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Collapse internal whitespace runs so a multi-paragraph task
	// becomes one tight line for the modal.
	fields := strings.Fields(s)
	joined := strings.Join(fields, " ")
	const maxLen = 200
	if len(joined) > maxLen {
		joined = joined[:maxLen-1] + "…"
	}
	return joined
}
