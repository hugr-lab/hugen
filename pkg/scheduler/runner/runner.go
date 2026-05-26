// Package runner is the agent-level scheduling primitive for the
// Phase 6 cron + scheduler subsystem. It owns a single goroutine
// that wakes on a fixed interval, walks every registered
// [RunnerFn], and dispatches the ones whose next planned fire has
// arrived. Failures are fault-isolated: a panicking or
// long-running fn cannot stall the tick loop or starve siblings.
//
// Runner has no DB. Internal services (session reapers, MCP
// reconnect, future memory pipeline) register their fns directly;
// the per-session TaskManager extension (Phase 6.1b) re-registers
// every user-defined task row at bootstrap. Either way Runner only
// keeps the live map plus an [RunnerRunLog] for fire history —
// every consumer rehydrates its registrations on its own boot.
//
// Tick latency is ~5s by default; sub-second precision is out of
// scope (§14). See design/004-runtime-post-phase-i/phase-6-spec.md
// §3 for the contract this package implements.
package runner

import (
	"context"
	"time"
)

// Runner is the public contract Phase 6 consumers depend on. The
// in-process implementation lives in [Service]; both internal
// services and the TaskManager extension consume the interface so
// future hub-mode swap to a remote scheduler can land without
// touching the call sites.
type Runner interface {
	// Register installs fn under name with sched as its cadence.
	// Registering an existing name replaces both the schedule and
	// the fn (idempotent — bootstrap paths may re-register on
	// every reattach without dedup). Returns an error only when
	// the runner has stopped.
	Register(ctx context.Context, name string, sched Schedule, fn RunnerFn, opts ...RegisterOption) error

	// Unregister removes the named registration. The currently
	// in-flight fire (if any) is allowed to finish; subsequent
	// ticks skip the name. Unknown names return nil.
	Unregister(ctx context.Context, name string) error

	// Pause flips a registration to paused. Ticks skip paused
	// fns; their schedules continue to be re-computed on Resume
	// so the cadence stays anchored to wall-clock time rather
	// than to "time spent paused".
	Pause(ctx context.Context, name string) error

	// Resume unpauses a registration. Next fire is computed by
	// calling Schedule.Next(now) so a long pause does not trigger
	// a burst of catch-up fires.
	Resume(ctx context.Context, name string) error

	// Status snapshots the named registration's state. Returns
	// ok=false when the name is not registered.
	Status(ctx context.Context, name string) (RunnerStatus, bool)

	// ListByPrefix returns every registration whose name starts
	// with prefix. Used by TaskManager extensions to enumerate
	// their owned tasks ("scheduler_task_*") and by operator UIs.
	ListByPrefix(prefix string) []RegisteredFn

	// Start launches the tick goroutine. Idempotent — calling
	// Start twice is a no-op. Stop must be called for clean
	// shutdown.
	Start(ctx context.Context) error

	// Stop signals the tick goroutine to exit and waits for any
	// in-flight fire goroutines to return. Safe to call multiple
	// times; the second call is a no-op.
	Stop(ctx context.Context) error
}

// RunnerFn is the work payload a registration dispatches at fire
// time. ctx is bounded by the per-registration timeout (default
// [DefaultFireTimeout]); fns SHOULD honour ctx.Done so the timeout
// path actually unblocks. Returning an error stamps the run-log
// entry with status=failed; the schedule continues regardless
// (fault isolation — see §3.1).
type RunnerFn func(ctx context.Context, fire FireMeta) (Outcome, error)

// FireMeta is the per-fire envelope passed into [RunnerFn]. It
// stays a small value struct so each tick can build one without
// allocations.
type FireMeta struct {
	// Name is the registration's name. Useful when one factory
	// produces multiple fns that share log lines.
	Name string

	// FireSeq is the 1-indexed fire counter for this name within
	// the current Runner lifetime. Resets to zero on
	// re-registration.
	FireSeq int

	// PlannedAt is the wall-clock instant the tick computed as
	// the fire's planned start. May lag the real clock by up to
	// one tick interval (typically ~5s).
	PlannedAt time.Time

	// PrevOutcome carries the most recent successful Outcome for
	// this name, or nil on the first fire after registration.
	// Sourced from the in-memory run-log.
	PrevOutcome *Outcome
}

// Outcome is the fn's structured return value. All fields are
// optional; reapers typically only fill Summary ("reaped N rows").
// TaskManager (Phase 6.1b) will fold StateDiff into the task's
// persistent state when present.
type Outcome struct {
	// Summary is a short one-line description ("reaped 3 rows").
	// Surfaced in run-log listings and operator UIs.
	Summary string

	// Body is a longer human-readable detail string. Empty for
	// most reapers; TaskManager fire fns put the notification
	// body here.
	Body string

	// StateDiff is a per-fire KV map. Reapers leave it nil;
	// TaskManager will populate it from mission-side
	// task:set_state calls (Phase 6.3).
	StateDiff map[string]any

	// ErrorMessage carries the error string for failed fires. The
	// canonical signal is the (Outcome, error) return tuple; this
	// field exists so the run-log can persist the message
	// independently of the Go error type.
	ErrorMessage string

	// Reason is a free-form category tag ("paused_skill_drift",
	// "user_cancel"). Used by TaskManager to render structured
	// reasons in the owner-session notification ExtensionFrame.
	Reason string

	// Usage is the model-token tally accumulated during the fire,
	// when the fn drove an LLM session. Reapers leave it nil.
	Usage *Usage
}

// Usage is the token usage rollup mirrored from the model layer.
// Phase 6.1a only declares the shape; the model package owns
// canonical accounting and will fill this in when TaskManager
// (Phase 6.1c) wires spawned cron sessions.
type Usage struct {
	InputTokens  int
	OutputTokens int
	TotalTokens  int
	Cost         float64
}

// Schedule decides the next fire instant given the current clock.
// Returning the zero time signals "no further fires" — the
// registration stays installed but ticks skip it. [Every] (every
// fixed interval) and [Once] (fire once at a specific instant)
// cover Phase 6.1a; cron-expression and human-prose schedules
// arrive with Phase 6.1b.
type Schedule interface {
	Next(after time.Time) time.Time
}

// RegisterOption tunes a single registration. The variadic surface
// keeps the [Runner.Register] call site readable when most
// registrations need no tweaking.
type RegisterOption func(*registerOptions)

type registerOptions struct {
	timeout   time.Duration
	startPaused bool
}

// WithFireTimeout overrides the per-fire context deadline.
// Defaults to [DefaultFireTimeout]. Pass 0 to disable the timeout
// entirely (use sparingly — a stuck fn pins one goroutine).
func WithFireTimeout(d time.Duration) RegisterOption {
	return func(o *registerOptions) {
		o.timeout = d
	}
}

// WithStartPaused registers the fn in paused state. Useful for
// retention reapers that ship dormant until the operator config
// activates them (Phase 6.3 §16.2).
func WithStartPaused() RegisterOption {
	return func(o *registerOptions) {
		o.startPaused = true
	}
}

// DefaultFireTimeout is the per-fire context deadline applied
// when [WithFireTimeout] is not passed. 30 minutes accommodates
// long-running mission spawns (Phase 6.1c) while still capping
// runaway fns.
const DefaultFireTimeout = 30 * time.Minute

// DefaultTickInterval is the wall-clock cadence at which the tick
// goroutine wakes. Override via [WithTickInterval] (typically only
// in tests) or the HUGEN_RUNNER_TICK_SECONDS env in Phase 6.1b.
const DefaultTickInterval = 5 * time.Second

// RunnerStatus snapshots a single registration. Returned by
// [Runner.Status] and embedded in [RegisteredFn].
type RunnerStatus struct {
	Paused      bool
	NextFireAt  time.Time
	LastFireAt  time.Time
	LastOutcome *Outcome
	FireCount   int
}

// RegisteredFn is one entry returned by [Runner.ListByPrefix].
// Carries the canonical name + the live Status; the [RunnerFn]
// itself is intentionally not exposed.
type RegisteredFn struct {
	Name   string
	Status RunnerStatus
}
