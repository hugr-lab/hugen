package mission

import "time"

// PlanState is the runtime's projection of plan progress through a
// mission's lifetime. Append-only by intent: every advance shifts
// the current wave from Active to Done; Roadmap is overwritten on
// each planner iteration with the planner's latest forecast.
//
// PlanState is read by Plan Executor, Plan Context renderer (phase
// D), and the StatusReporter capability. Persistence is via
// session_events under the mission ext's namespace; the in-memory
// copy is the projection of those events.
type PlanState struct {
	// Done lists every wave that completed (any status — ok,
	// partial, failed). Ordered by completion time. Phase 5.2's
	// compactor may eventually trim this; v1 keeps every entry.
	Done []DoneWave `json:"done,omitempty"`

	// Active is the wave currently running (workers spawned, not
	// all settled). Nil when no wave is in flight (between planner
	// iterations, or after synthesis).
	Active *Wave `json:"active,omitempty"`

	// Roadmap is the planner's latest forecast — upcoming waves
	// (label + one-line description) the planner expects to
	// follow Active. Overwritten on every planner iteration.
	// Model-readable + rendered into the approval inquire question
	// so the user sees the bigger picture before approving the
	// immediate next wave.
	Roadmap []RoadmapEntry `json:"roadmap,omitempty"`

	// Iteration counts planner cycles completed. Increments on
	// every planner_invalid retry exhaustion or wave_complete that
	// led to a new planner spawn. Used as the approval-iteration
	// cap (spec §0.5) and for observability.
	Iteration int `json:"iteration,omitempty"`
}

// DoneWave is the immutable record of a completed wave. Refs lists
// every handoff ref the wave produced (one per non-errored
// subagent); used for the [Plan state] catalog rendered into
// planner / checker / synthesizer first messages.
type DoneWave struct {
	Label       string       `json:"label"`
	Status      WaveStatus   `json:"status"`
	CompletedAt time.Time    `json:"completed_at"`
	Refs        []string     `json:"refs,omitempty"`
	Subagents   []DoneWorker `json:"subagents,omitempty"`
}

// DoneWorker is the per-worker terminal record within a Done wave.
// Status is one of {ok, error, timeout, cancelled}; Ref points into
// the Handoffs store for ok workers.
type DoneWorker struct {
	Name   string `json:"name"`
	Role   string `json:"role,omitempty"`
	Skill  string `json:"skill,omitempty"`
	Status string `json:"status"`
	Ref    string `json:"ref,omitempty"`
	Error  string `json:"error,omitempty"`
	// TimedOut marks a worker the executor cancelled for overrunning
	// its per-role time budget — distinct from a generic error so the
	// planner can react (split the work vs redo whole) on a timeout
	// specifically. Status is "timeout" when this is set.
	TimedOut bool `json:"timed_out,omitempty"`
}

// WaveStatus is the aggregate outcome of a wave. Phase A only ever
// produces ok/failed (no checker-driven partial yet); phase C
// extends with "partial" when some workers error but at least one
// succeeded.
type WaveStatus string

const (
	WaveStatusOk      WaveStatus = "ok"
	WaveStatusPartial WaveStatus = "partial"
	WaveStatusFailed  WaveStatus = "failed"
)
