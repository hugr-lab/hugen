package mission

import (
	"context"
	"fmt"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
)

// SpawnRequest is the spawner-callback payload. Mirrors the
// fields pkg/session.SpawnSpec needs without importing pkg/session
// (strict layering — pkg/session imports pkg/extension, never
// the reverse).
//
// RenderMode carries the protocol.SubagentRender* constant value
// the executor wants applied to the child's terminal SubagentResult
// projection; for Phase A the executor always sets this to
// "silent" so the mission supervisor's history isn't polluted by
// per-worker terminal frames.
type SpawnRequest struct {
	Name       string
	Skill      string
	Role       string
	Task       string
	Inputs     any
	RenderMode string
}

// SpawnResult describes a freshly-spawned worker. SessionID is the
// child's id (mission ext stores this against the workerCursor so
// ChildFrameObserver can attribute terminal frames back). Settled
// closes when the child's first user message has landed in its
// inbox — the executor waits on it before declaring the worker
// "started".
type SpawnResult struct {
	SessionID string
	Settled   <-chan struct{}
}

// Spawner is the runtime-supplied callback the executor uses to
// open worker sub-sessions. The integration wiring (cmd/hugen +
// pkg/runtime) closes over a *session.Session reference and
// forwards the request to its Spawn method.
//
// Returning an error halts the wave with WaveStatus=failed.
type Spawner func(ctx context.Context, parent extension.SessionState, req SpawnRequest) (SpawnResult, error)

// Terminator stops a worker sub-session by its id. The mission ext
// wires it to the parent session's CancelChild. RunWave calls it when
// a worker overruns its per-role time budget — without it a worker
// runs DETACHED (context.WithoutCancel) to completion and its result
// is orphaned (the wave already moved on). Optional: a nil terminator
// logs and skips cancellation (the worker still gets a `timeout`
// outcome, it just isn't actually stopped).
type Terminator func(ctx context.Context, sessionID, reason string) error

// Executor is the Plan Executor primitive — Phase A exposes only
// RunWave (one parallel batch + wait + parse). Iteration over a
// full Plan is the planner's job (Phase B).
//
// Executor is stateless across waves; per-mission state lives on
// the [*MissionState] handle reachable via state.Value(StateKey).
type Executor struct {
	spawner    Spawner
	terminator Terminator
	logger     loggerLike
	// waveHook, when set, fires on every wave transition (start +
	// completion) with the mission session. Prod wiring points it at
	// the Extension's spec.md snapshot writer so the on-disk plan
	// tracks done/doing live; tests leave it nil. Phase B39.
	waveHook func(extension.SessionState)
}

// NewExecutor constructs an Executor backed by spawner. logger may
// be nil; the executor falls back to a no-op logger. The worker-cancel
// terminator is optional — set it via [Executor.WithTerminator].
func NewExecutor(spawner Spawner, logger loggerLike) *Executor {
	if logger == nil {
		logger = noopLogger{}
	}
	return &Executor{spawner: spawner, logger: logger}
}

// WithTerminator sets the worker-cancel callback RunWave uses to stop
// a worker that overran its per-role time budget. Returns the executor
// for chaining. Same-package prod wiring sets this; tests may leave it
// nil (a timed-out worker then gets a `timeout` outcome but is not
// actually cancelled).
func (e *Executor) WithTerminator(t Terminator) *Executor {
	e.terminator = t
	return e
}

// WithWaveHook sets the wave-transition callback RunWave fires at
// wave start (after Active is stamped) and wave completion (after the
// wave folds into Done). Prod wiring points it at the spec.md snapshot
// writer; returns the executor for chaining. Phase B39.
func (e *Executor) WithWaveHook(h func(extension.SessionState)) *Executor {
	e.waveHook = h
	return e
}

// RunWaveOptions tunes per-wave executor behaviour.
type RunWaveOptions struct {
	// RoleTimeout returns the wall-clock budget for a worker of the
	// given role. RunWave gives EACH worker its OWN deadline from
	// this — a worker that overruns is cancelled (via the executor's
	// terminator) and marked `timeout`, while its siblings keep
	// running and the wave collects partial results. This replaces
	// the old single wave-level timeout (= max across roles) that
	// abandoned the whole wave on the slowest worker's budget. Nil
	// falls back to DefaultWaveTimeout for every worker.
	RoleTimeout func(role string) time.Duration

	// RenderMode overrides the executor's per-worker default of
	// protocol.SubagentRenderSilent. Set to "" to keep the default.
	RenderMode string
}

// RunWave spawns every subagent in wave in parallel, registers
// each with the mission state so [ChildFrameObserver] can
// attribute their terminal frames, and waits until every worker
// has produced a handoff (any status — ok / error / partial).
//
// Returns the per-worker outcome list and the wave's aggregate
// WaveStatus once every worker has settled. Surfaces a context
// error if ctx fires before completion; the wave is then left
// in the partial state and the executor may be retried by the
// caller.
//
// Phase A: depends_on resolution happens BEFORE spawn — refs are
// looked up in the store and the worker's task is prefixed with
// the [Resolved depends_on] section. Forward references / unknown
// refs cause the wave to fail-fast without spawning anyone.
func (e *Executor) RunWave(ctx context.Context, state extension.SessionState, wave Wave, opts RunWaveOptions) (WaveStatus, []DoneWorker, error) {
	if state == nil {
		return WaveStatusFailed, nil, fmt.Errorf("mission.RunWave: state is nil")
	}
	if e.spawner == nil {
		return WaveStatusFailed, nil, fmt.Errorf("mission.RunWave: spawner is nil")
	}
	m := FromState(state)
	if m == nil {
		return WaveStatusFailed, nil, fmt.Errorf("mission.RunWave: no MissionState on session")
	}
	if wave.Label == "" {
		return WaveStatusFailed, nil, fmt.Errorf("mission.RunWave: wave.Label is empty")
	}
	if len(wave.Subagents) == 0 {
		return WaveStatusFailed, nil, fmt.Errorf("mission.RunWave: wave %q has no subagents", wave.Label)
	}

	// Pre-resolve every depends_on against the current Handoffs
	// store. Failure here fails the wave without any spawn — this
	// keeps the partial-state surface minimal.
	resolved := make(map[string]string, len(wave.Subagents))
	for _, sub := range wave.Subagents {
		body, err := ResolveDependsOn(sub.DependsOn, m.Handoffs)
		if err != nil {
			return WaveStatusFailed, nil, fmt.Errorf("wave %q subagent %q: %w", wave.Label, sub.Name, err)
		}
		resolved[sub.Name] = body
	}

	// Mark wave active so the observer attributes incoming frames.
	// BeginWave also stamps PlanState.Active; fire the wave hook so
	// the spec.md snapshot shows the wave as "doing" while it runs.
	m.BeginWave(wave)
	if e.waveHook != nil {
		e.waveHook(state)
	}

	renderMode := opts.RenderMode
	if renderMode == "" {
		renderMode = "silent"
	}

	// Each worker gets its OWN wall-clock budget by role (NOT one
	// wave-level deadline). A worker that overruns is cancelled +
	// marked `timeout` below; its siblings keep running.
	roleTimeout := opts.RoleTimeout
	if roleTimeout == nil {
		roleTimeout = func(string) time.Duration { return DefaultWaveTimeout }
	}

	// Spawn every worker. Naming is the responsibility of the
	// caller-supplied spec; collision-suffix happens inside
	// pkg/session.Spawn.
	records := make([]spawnRecord, len(wave.Subagents))
	for i, sub := range wave.Subagents {
		task := sub.Task
		if resolved[sub.Name] != "" {
			task = resolved[sub.Name] + "\n\n" + task
		}
		req := SpawnRequest{
			Name:       sub.Name,
			Skill:      sub.Skill,
			Role:       sub.Role,
			Task:       task,
			Inputs:     sub.Inputs,
			RenderMode: renderMode,
		}
		res, err := e.spawner(ctx, state, req)
		records[i] = spawnRecord{spec: sub, res: res, err: err}
		if err != nil {
			e.logger.Warn("mission: RunWave: spawn failed",
				"wave", wave.Label, "subagent", sub.Name, "err", err)
			continue
		}
		m.RegisterWorker(res.SessionID, workerCursor{
			Name:  sub.Name,
			Role:  sub.Role,
			Skill: sub.Skill,
		})
	}

	// Wait for every spawned worker, each under its OWN per-role
	// deadline. The observer puts handoffs into m.Handoffs keyed by
	// the canonical ref; the executor polls for ref-presence under a
	// short backoff. A worker that passes its deadline without a
	// handoff is cancelled (terminator) and recorded as `timeout`,
	// while its siblings keep being awaited — so one slow worker no
	// longer abandons the whole wave.
	deadlineBase := nowFn()
	pending := make([]pendingWorker, 0, len(wave.Subagents))
	for _, rec := range records {
		if rec.err != nil {
			continue
		}
		ref, _ := MakeRef(rec.spec.Name, wave.Label)
		pending = append(pending, pendingWorker{
			ref:       ref,
			sessionID: rec.res.SessionID,
			deadline:  deadlineBase.Add(roleTimeout(rec.spec.Role)),
		})
	}

	timedOut, waitErr := e.waitForWorkers(ctx, m.Handoffs, pending)
	if waitErr != nil {
		// Parent ctx fired (mission-level cancel / shutdown) — return
		// what we have; the timeout map still flags any worker we
		// terminated before the ctx cut us off. Clear Active + fire the
		// wave hook so a cancelled mission's spec.md doesn't strand a
		// phantom "active wave" (the completion block below — which
		// normally clears Active and re-snapshots — is skipped on this
		// early return).
		m.mu.Lock()
		m.Plan.Active = nil
		m.currentWave = ""
		m.mu.Unlock()
		if e.waveHook != nil {
			e.waveHook(state)
		}
		return WaveStatusFailed, collectOutcomes(records, wave.Label, m.Handoffs, timedOut), waitErr
	}

	// Aggregate outcomes + status.
	outcomes := collectOutcomes(records, wave.Label, m.Handoffs, timedOut)
	status := aggregateStatus(outcomes)

	// Record the wave under Done on PlanState.
	m.mu.Lock()
	refs := make([]string, 0, len(outcomes))
	for _, w := range outcomes {
		if w.Ref != "" {
			refs = append(refs, w.Ref)
		}
	}
	m.Plan.Done = append(m.Plan.Done, DoneWave{
		Label:       wave.Label,
		Status:      status,
		CompletedAt: nowFn(),
		Refs:        refs,
		Subagents:   outcomes,
	})
	m.Plan.Active = nil
	m.currentWave = ""
	m.mu.Unlock()

	// Wave folded into Done + Active cleared — re-project the spec.md
	// snapshot so the on-disk plan shows the completed wave. Fired
	// outside m.mu (the snapshot writer takes the lock itself).
	if e.waveHook != nil {
		e.waveHook(state)
	}

	return status, outcomes, nil
}

// pendingWorker is one in-flight worker the wait loop tracks: its
// canonical handoff ref, its session id (for cancellation), and its
// own per-role deadline.
type pendingWorker struct {
	ref       string
	sessionID string
	deadline  time.Time
}

// waitForWorkers polls the store until every pending worker is either
// DONE (its handoff ref landed) or TIMED OUT (its own deadline passed
// → it is cancelled via the executor's terminator). When one worker
// times out the others keep being awaited — a slow worker no longer
// abandons the whole wave. Returns the set of refs that timed out. The
// only early exit is the parent ctx firing (mission-level cancel /
// shutdown), which returns ctx.Err() alongside whatever timed out so
// far. Phase B will replace the poll with an observer completion
// channel.
func (e *Executor) waitForWorkers(ctx context.Context, store *Handoffs, workers []pendingWorker) (map[string]bool, error) {
	timedOut := make(map[string]bool)
	if len(workers) == 0 {
		return timedOut, nil
	}
	done := make(map[string]bool)
	const pollEvery = 50 * time.Millisecond
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	for {
		remaining := 0
		now := nowFn()
		for _, w := range workers {
			if done[w.ref] || timedOut[w.ref] {
				continue
			}
			if _, ok := store.Get(w.ref); ok {
				done[w.ref] = true
				continue
			}
			if now.After(w.deadline) {
				timedOut[w.ref] = true
				e.cancelTimedOutWorker(ctx, w)
				continue
			}
			remaining++
		}
		if remaining == 0 {
			return timedOut, nil
		}
		select {
		case <-ctx.Done():
			return timedOut, ctx.Err()
		case <-ticker.C:
		}
	}
}

// cancelTimedOutWorker stops a worker that overran its budget. Workers
// run detached (context.WithoutCancel), so cancellation MUST go through
// the explicit terminator — without one the worker runs to completion
// and orphans its result. We still mark it timed out either way.
func (e *Executor) cancelTimedOutWorker(ctx context.Context, w pendingWorker) {
	if e.terminator == nil || w.sessionID == "" {
		e.logger.Warn("mission: RunWave: worker exceeded its time budget but was NOT cancelled (no terminator) — it runs detached, result orphaned",
			"ref", w.ref, "session", w.sessionID)
		return
	}
	e.logger.Warn("mission: RunWave: worker exceeded its per-role time budget — cancelling",
		"ref", w.ref, "session", w.sessionID)
	if err := e.terminator(ctx, w.sessionID, "mission: worker exceeded its per-role time budget"); err != nil {
		e.logger.Warn("mission: RunWave: cancel of timed-out worker failed",
			"ref", w.ref, "session", w.sessionID, "err", err)
	}
}

// spawnRecord is the per-worker bookkeeping the RunWave loop
// builds at spawn-time and re-uses at outcome-collection time.
type spawnRecord struct {
	spec SubagentSpec
	res  SpawnResult
	err  error
}

// collectOutcomes maps each spawn record to a DoneWorker. timedOut
// carries the refs the wait loop cancelled for overrunning their
// budget — they surface as Status "timeout" / TimedOut so the planner
// can react (split vs redo) distinctly from a generic failure. A real
// handoff that landed despite a timeout race still wins.
func collectOutcomes(records []spawnRecord, waveLabel string, store *Handoffs, timedOut map[string]bool) []DoneWorker {
	out := make([]DoneWorker, 0, len(records))
	for _, rec := range records {
		entry := DoneWorker{
			Name:  rec.spec.Name,
			Role:  rec.spec.Role,
			Skill: rec.spec.Skill,
		}
		if rec.err != nil {
			entry.Status = "error"
			entry.Error = rec.err.Error()
			out = append(out, entry)
			continue
		}
		ref, _ := MakeRef(rec.spec.Name, waveLabel)
		entry.Ref = ref
		switch h, ok := store.Get(ref); {
		case ok:
			entry.Status = h.Status
			if h.Status != "ok" {
				entry.Error = h.Reason
			}
		case timedOut[ref]:
			entry.Status = "timeout"
			entry.TimedOut = true
			entry.Error = "worker exceeded its per-role time budget"
		default:
			entry.Status = "error"
			entry.Error = "no handoff produced before wave ctx expired"
		}
		out = append(out, entry)
	}
	return out
}

func aggregateStatus(outcomes []DoneWorker) WaveStatus {
	ok, bad := 0, 0
	for _, w := range outcomes {
		switch w.Status {
		case "ok":
			ok++
		default:
			bad++
		}
	}
	switch {
	case ok == 0:
		return WaveStatusFailed
	case bad == 0:
		return WaveStatusOk
	default:
		return WaveStatusPartial
	}
}

// loggerLike is the minimal logging surface the executor uses,
// kept narrow so callers can supply a *slog.Logger or any tiny
// adapter. Matches the methods the mission ext's own logger uses.
type loggerLike interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

type noopLogger struct{}

func (noopLogger) Debug(string, ...any) {}
func (noopLogger) Info(string, ...any)  {}
func (noopLogger) Warn(string, ...any)  {}
