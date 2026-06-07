package mission

import (
	"fmt"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
)

// approvalQuestionView is the typed payload the
// `mission/approval_question` template renders against. The
// AcceptanceCriteriaDiff field is the structured per-row view
// (id, statement, status, change tag) the modal renders with
// status icons + [NEW]/[EDITED]/[DROPPED] markers (§3.6); the
// `MissionAcceptanceCriteria` flat slice survives only as the
// pre-diff fallback for callers that haven't migrated to the
// structured renderer.
type approvalQuestionView struct {
	MissionGoal               string
	MissionAcceptanceCriteria []string
	AcceptanceCriteriaDiff    []ACViewEntry
	NextWave                  approvalWaveView
	WaveAcceptanceCriteria    []string
	Roadmap                   []RoadmapEntry
	Rationale                 string
}

// ACViewEntry is one row of the structured diff the approval modal
// renders. Carries the post-apply contract (statement / status)
// PLUS a `Change` tag describing how this row got there relative
// to state.AC pre-diff. Phase 5.x — B11 §3.6.
type ACViewEntry struct {
	// ID is the stable row id (`ac-N`). Empty only for entries the
	// planner-staged ac_add hasn't yet been assigned an id (modal
	// renders without the id prefix in that case).
	ID string

	// Statement is the row's text, post-diff (i.e. with planner's
	// statement rewrite already applied).
	Statement string

	// Status is the row's status, post-diff. One of
	// unsatisfied / satisfied / dropped.
	Status ACStatus

	// Change tags how this row relates to the pre-diff state. One of
	// `carry` (unchanged), `edited` (statement rewritten), `new`
	// (added in this diff), `dropped` (dropped in this diff).
	Change string

	// DropReason carries the diff's drop_reason when Change=dropped.
	DropReason string
}

const (
	ACChangeCarry   = "carry"
	ACChangeEdited  = "edited"
	ACChangeNew     = "new"
	ACChangeDropped = "dropped"
)

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
//
// The mission-AC bullet list is the projection-after-diff: state.AC
// statements (excluding dropped rows) WITH the planner's staged
// diff (ac_add / ac_update) overlaid as if the user clicked Approve.
// That way the user reads the contract they're about to sign, not
// the pre-diff list. Structured diff rendering (status icons + per-
// row [NEW] / [EDITED] / [DROPPED] markers) lands separately in ζ.
//
// Phase 5.x — B11 §3.6.
func renderApprovalQuestion(mission extension.SessionState, plan Plan) (string, error) {
	view := approvalQuestionView{
		MissionGoal:               strings.TrimSpace(plan.MissionGoal),
		MissionAcceptanceCriteria: projectACForApproval(mission, plan),
		AcceptanceCriteriaDiff:    projectACDiffViewForApproval(mission, plan),
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

// projectACDiffViewForApproval builds the structured per-row diff
// view the approval modal renders with icons + change tags
// ([NEW]/[EDITED]/[DROPPED]). Each entry carries the post-apply
// statement + status + change marker.
//
// Algorithm (§3.6):
//
//   - For every row currently in state.AC, start with Change=carry.
//   - Apply plan.ACUpdate overlays: statement rewrite → Change=edited;
//     drop → Change=dropped (with reason).
//   - Append plan.ACAdd entries at the bottom with Change=new (no
//     id yet — the id mints at commit time).
//
// Dropped rows render at the bottom of the kept list (after
// edited / carry, before new) so the modal reads top-to-bottom as
// "this is what stays / changes / disappears / is added".
//
// Returns nil when state is unreachable AND the plan carries no
// diff — the modal then falls back to the flat
// MissionAcceptanceCriteria slice (legacy renderer).
func projectACDiffViewForApproval(mission extension.SessionState, plan Plan) []ACViewEntry {
	m := FromState(mission)
	planUpdatesByID := make(map[string]ACUpdateSpec, len(plan.ACUpdate))
	for _, u := range plan.ACUpdate {
		planUpdatesByID[strings.TrimSpace(u.ID)] = u
	}
	var carry, edited, dropped, news []ACViewEntry
	if m != nil {
		for _, row := range m.ACSnapshot() {
			entry := ACViewEntry{
				ID:        row.ID,
				Statement: row.Statement,
				Status:    row.Status,
				Change:    ACChangeCarry,
			}
			if row.Status == ACDropped {
				entry.Change = ACChangeDropped
				entry.DropReason = row.DropReason
				dropped = append(dropped, entry)
				continue
			}
			if u, ok := planUpdatesByID[row.ID]; ok {
				if u.Drop {
					entry.Status = ACDropped
					entry.Change = ACChangeDropped
					entry.DropReason = strings.TrimSpace(u.DropReason)
					dropped = append(dropped, entry)
					continue
				}
				if u.Statement != "" {
					entry.Statement = strings.TrimSpace(u.Statement)
					entry.Change = ACChangeEdited
					edited = append(edited, entry)
					continue
				}
				// status-only update on a carried row — render under
				// carry with the updated status.
				if u.Status != "" {
					entry.Status = u.Status
				}
			}
			carry = append(carry, entry)
		}
	}
	for _, a := range plan.ACAdd {
		stmt := strings.TrimSpace(a.Statement)
		if stmt == "" {
			continue
		}
		news = append(news, ACViewEntry{
			Statement: stmt,
			Status:    ACUnsatisfied,
			Change:    ACChangeNew,
		})
	}
	if len(carry)+len(edited)+len(dropped)+len(news) == 0 {
		return nil
	}
	out := make([]ACViewEntry, 0, len(carry)+len(edited)+len(dropped)+len(news))
	out = append(out, carry...)
	out = append(out, edited...)
	out = append(out, news...)
	out = append(out, dropped...)
	return out
}

// projectACForApproval renders the bullet list shown in the
// approval modal: the result of applying plan.ACAdd + plan.ACUpdate
// over state.AC, in insertion order, dropping rows the diff would
// drop. The diff is applied virtually — neither state.AC nor the
// staging slot is mutated by this call (the actual commit happens
// in CommitStagedDiff once the user clicks Approve).
//
// State pulled via FromState(mission). Returns an empty slice when
// no state handle is reachable (defensive — should not happen for a
// mission session that reached the validate_and_approve tool).
func projectACForApproval(mission extension.SessionState, plan Plan) []string {
	m := FromState(mission)
	if m == nil {
		// Defensive: surface the planner's staged ac_add bullets so
		// the modal at least shows what's being added. Existing AC
		// is unreachable from this branch.
		out := make([]string, 0, len(plan.ACAdd))
		for _, a := range plan.ACAdd {
			if s := strings.TrimSpace(a.Statement); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	current := m.ACSnapshot()
	// Apply ac_update overlay first so the rendered bullets reflect
	// the proposed contract. Index by id for lookup.
	byID := make(map[string]int, len(current))
	for i, row := range current {
		byID[row.ID] = i
	}
	for _, u := range plan.ACUpdate {
		idx, ok := byID[strings.TrimSpace(u.ID)]
		if !ok {
			continue // stage validator already rejected; defensive skip here
		}
		row := current[idx]
		if u.Statement != "" {
			row.Statement = strings.TrimSpace(u.Statement)
		}
		if u.Drop {
			row.Status = ACDropped
			if u.DropReason != "" {
				row.DropReason = strings.TrimSpace(u.DropReason)
			}
		} else if u.Status != "" {
			row.Status = u.Status
		}
		current[idx] = row
	}
	out := make([]string, 0, len(current)+len(plan.ACAdd))
	for _, row := range current {
		if row.Status == ACDropped {
			continue
		}
		if stmt := strings.TrimSpace(row.Statement); stmt != "" {
			out = append(out, stmt)
		}
	}
	for _, a := range plan.ACAdd {
		if s := strings.TrimSpace(a.Statement); s != "" {
			out = append(out, s)
		}
	}
	return out
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
