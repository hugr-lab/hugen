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

// Plan is the structured representation of a mission plan. The
// canonical source is a planner LLM subagent (phase B+) that emits
// a YAML/JSON fenced block conforming to output_contract.kind=plan;
// for Phase A the same shape is hardcoded inside a fixture skill
// manifest under mission.plan.experimental_inline.
//
// Plan is intentionally small: NextWave is the one wave the executor
// runs next, Roadmap is the planner's high-level intent for what's
// to come (model-readable only, not auto-executed), and Rationale is
// the prose explaining why this wave was picked. The combination
// supports iterative replanning — every cycle the planner emits a
// fresh Plan based on prior wave's outputs.
type Plan struct {
	// NextWave is the wave the executor will run immediately. Phase
	// A only ever has exactly one wave (hardcoded); phase B onwards
	// the planner emits one fresh Plan per iteration.
	NextWave Wave `json:"next_wave" yaml:"next_wave"`

	// Roadmap lists labels of waves the planner anticipates after
	// NextWave. Model-visible hint only; runtime does NOT auto-run
	// them. Empty when the planner thinks NextWave finishes the
	// mission.
	Roadmap []string `json:"roadmap,omitempty" yaml:"roadmap,omitempty"`

	// Rationale is the planner's free-form justification. Renders
	// into the [Plan context] section of downstream phase roles'
	// first message (phase D).
	Rationale string `json:"rationale,omitempty" yaml:"rationale,omitempty"`
}

// Wave is one parallel batch of subagent spawns sharing a label.
// The label is the human-readable wave identifier used in handoff
// refs ("<subagent_name>@<wave_label>"); for Phase A's
// experimental_inline shape the skill author picks the label
// directly.
type Wave struct {
	// Label is the unique-per-mission wave identifier. Required.
	// Canonical form: kebab-case ("schema-discovery", "analysis").
	Label string `json:"label" yaml:"label"`

	// Subagents lists the workers spawned in parallel for this wave.
	// Order is not significant for execution (parallel) but is
	// preserved in the Plan AST for readability and for the
	// scenario harness's by-eye assertions.
	Subagents []SubagentSpec `json:"subagents" yaml:"subagents"`
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
