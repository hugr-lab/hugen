// Package mission implements the mission-PDCA orchestration runtime
// as a capability-bag session extension. Phase A — skeleton only:
// types for the Plan AST, the per-mission Handoffs store, basic
// output_contract shape checks, and the Plan Executor's RunWave
// primitive. The LLM-driven planner / checker / synthesizer paths
// land in phases B–D.
//
// Mission orchestration lives entirely in this package. The session
// runtime (pkg/session) is reached only through existing capability
// interfaces from pkg/extension; the only pkg/session edit is the
// SpawnSpec.RenderMode plumbing that lets external callers request
// SubagentRenderSilent without poking session internals.
//
// See design/003-mission-pdca/design.md (canon) and
// design/003-mission-pdca/spec.md §3 Phase A for scope.
package mission

import "encoding/json"

var jsonUnmarshal = json.Unmarshal

// Plan is the structured representation of a mission plan. The
// canonical source is a planner LLM subagent that emits a YAML/JSON
// fenced block conforming to output_contract.kind=plan; the same
// shape can also be hardcoded inside a fixture or task skill
// manifest under mission.plan.inline.
//
// Plan is intentionally small: NextWave is the one wave the executor
// runs next, Roadmap is the planner's high-level intent for what's
// to come (model-readable only, not auto-executed), and Rationale is
// the prose explaining why this wave was picked. The combination
// supports iterative replanning — every cycle the planner emits a
// fresh Plan based on prior wave's outputs.
type Plan struct {
	// MissionGoal is the planner's CURRENT WORKING UNDERSTANDING of
	// what the mission is delivering. Distinct from the raw user
	// goal text — the planner restates it each iteration, folding in
	// research findings / refine-loop feedback / mission:notify
	// follow-ups. Lets the checker compare "what user said" vs "what
	// planner thinks we're doing" and flag drift. Required when
	// `next_wave` is non-null (i.e. the mission isn't already in
	// plan_complete state). Phase I.26.
	MissionGoal string `json:"mission_goal,omitempty" yaml:"mission_goal,omitempty"`

	// ACAdd is the planner's per-iter ac_add diff: new acceptance
	// criteria the planner proposes for this iteration. Runtime mints
	// an `ac-N` id on apply, stamps origin `planner_iter_N` (unless
	// the entry carries its own — e.g. `research_proposal` for AC
	// promoted from the research findings). Phase 5.x — B11 §3.2.
	//
	// Empty on iters where the planner doesn't introduce new
	// criteria. On iteration 1 the planner SHOULD emit ≥1 ac_add
	// entry OR the mission must have manifest-seeded AC; an empty
	// AC set on iter 1 fails the runtime's pre-execution check.
	//
	// Every ac_add triggers approval-modal reopen (contract change
	// per §3.2.1).
	ACAdd []ACAddSpec `json:"ac_add,omitempty" yaml:"ac_add,omitempty"`

	// ACUpdate is the planner's per-iter ac_update diff: changes to
	// existing rows (statement rewrite, drop, status). Status-only
	// entries apply immediately; statement / drop entries are staged
	// and applied only after the user closes the approval modal.
	// Phase 5.x — B11 §3.2.
	ACUpdate []ACUpdateSpec `json:"ac_update,omitempty" yaml:"ac_update,omitempty"`

	// NextWave is the wave the executor will run immediately. Phase
	// A only ever has exactly one wave (hardcoded); phase B onwards
	// the planner emits one fresh Plan per iteration.
	NextWave Wave `json:"next_wave" yaml:"next_wave"`

	// Roadmap lists upcoming waves the planner anticipates after
	// NextWave. Model-visible hint only; runtime does NOT auto-run
	// them. Each entry pairs a kebab-case label with a one-line
	// description so the approval inquire and downstream prompts
	// can surface the plan beyond NextWave to the user — older
	// `[]string` payloads still parse (UnmarshalJSON tolerates the
	// legacy shape) and decode to entries with empty Description.
	Roadmap []RoadmapEntry `json:"roadmap,omitempty" yaml:"roadmap,omitempty"`

	// Rationale is the planner's free-form justification. Renders
	// into the [Plan context] section of downstream phase roles'
	// first message (phase D).
	Rationale string `json:"rationale,omitempty" yaml:"rationale,omitempty"`

	// RequiresReapproval signals that the planner believes the
	// mission contract changed materially since the user last
	// approved (goal reframed, AC added/dropped, new constraint
	// surfaced via refine-loop or worker handoff). When true on a
	// non-first iteration, `mission:validate_and_approve` re-opens
	// the approval modal even though the user already signed off on
	// an earlier plan. When false, subsequent iterations pass
	// silently as long as no worker handoff invalidated the prior
	// approval. Phase 5.x — B13 supersedes the sha256 frame-hashing
	// gate that re-prompted on cosmetic wording drift.
	RequiresReapproval bool `json:"requires_reapproval,omitempty" yaml:"requires_reapproval,omitempty"`

	// ReapprovalReason is the planner's one-line explanation of why
	// this iteration sets RequiresReapproval. Surfaces in the
	// approval modal's context so the user understands what changed
	// vs the previous approval. Ignored when RequiresReapproval is
	// false.
	ReapprovalReason string `json:"reapproval_reason,omitempty" yaml:"reapproval_reason,omitempty"`
}

// Wave is one parallel batch of subagent spawns sharing a label.
// The label is the human-readable wave identifier used in handoff
// refs ("<subagent_name>@<wave_label>"); for the inline plan shape
// the skill author picks the label directly.
type Wave struct {
	// Label is the unique-per-mission wave identifier. Required.
	// Canonical form: kebab-case ("schema-discovery", "analysis").
	Label string `json:"label" yaml:"label"`

	// Subagents lists the workers spawned in parallel for this wave.
	// Order is not significant for execution (parallel) but is
	// preserved in the Plan AST for readability and for the
	// scenario harness's by-eye assertions.
	Subagents []SubagentSpec `json:"subagents" yaml:"subagents"`

	// SkipCheck, when true on a planner-emitted next_wave, lets the
	// runtime skip the checker spawn for this wave on the success
	// path. The planner sets it for trivial waves whose verdict is
	// obvious (one worker, one query, status=ok → continue). On
	// wave failure the synthetic verdict-amend path still fires
	// regardless of SkipCheck — failures always replan.
	SkipCheck bool `json:"skip_check,omitempty" yaml:"skip_check,omitempty"`

	// AcceptanceCriteria narrows the wave-level "done" definition:
	// the checker reads them and emits per-criterion satisfied
	// flags in the verdict body. Optional — empty means the checker
	// just sanity-checks the wave's handoffs against the mission AC
	// without a wave-specific frame. Phase I.26.
	AcceptanceCriteria []string `json:"acceptance_criteria,omitempty" yaml:"acceptance_criteria,omitempty"`
}

// SubagentSpec is one worker entry within a wave. Mirrors the
// SpawnSpec fields the runtime requires (Name, Skill, Role, Task,
// Inputs) plus the mission-only DependsOn graph.
type SubagentSpec struct {
	// Name is the worker's short identifier within this wave —
	// becomes part of the handoff ref ("<name>@<wave_label>").
	// Required, kebab-case, unique within the wave.
	Name string `json:"name" yaml:"name"`

	// Skill names the skill providing the role; empty falls back to
	// the mission's own dispatching skill.
	Skill string `json:"skill,omitempty" yaml:"skill,omitempty"`

	// Role is the role within Skill. Required for skills that
	// declare multiple roles; optional for single-role skills.
	Role string `json:"role,omitempty" yaml:"role,omitempty"`

	// Task is the worker's first-message brief. May embed Go-template
	// expressions {{ .Inputs.X }} resolved against the mission's
	// Inputs map at executor time.
	Task string `json:"task" yaml:"task"`

	// Inputs is structured JSON the worker sees alongside its task.
	// Per-worker; merged into the worker's first-message [Inputs]
	// section.
	Inputs any `json:"inputs,omitempty" yaml:"inputs,omitempty"`

	// DependsOn lists handoff refs from earlier waves this worker
	// needs verbatim in its first message under [Resolved depends_on].
	// Format: "<subagent_name>@<wave_label>". Cyclical or
	// forward-pointing refs are rejected at executor parse time.
	DependsOn []string `json:"depends_on,omitempty" yaml:"depends_on,omitempty"`
}

// RoadmapEntry is one upcoming-wave hint emitted alongside the
// planner's NextWave. Carries the wave label plus a one-line
// description so the runtime-driven approval inquire can render
// the full plan beyond NextWave (the user sees the bigger
// picture before approving). Description is optional but
// strongly recommended — without it the inquire only shows
// labels.
type RoadmapEntry struct {
	Label       string `json:"label" yaml:"label"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

// UnmarshalJSON tolerates both the typed shape
// `{"label":"…","description":"…"}` and the legacy bare-string
// shape `"label"` so an older planner emitting a flat
// `[]string` roadmap still parses cleanly. The bare-string path
// produces an entry with Description="" — the runtime then
// renders the label only.
func (r *RoadmapEntry) UnmarshalJSON(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if data[0] == '"' {
		var s string
		if err := jsonUnmarshal(data, &s); err != nil {
			return err
		}
		r.Label = s
		r.Description = ""
		return nil
	}
	type roadmapAlias RoadmapEntry
	var alias roadmapAlias
	if err := jsonUnmarshal(data, &alias); err != nil {
		return err
	}
	*r = RoadmapEntry(alias)
	return nil
}
