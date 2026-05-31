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

// Executor is the Plan Executor primitive — Phase A exposes only
// RunWave (one parallel batch + wait + parse). Iteration over a
// full Plan is the planner's job (Phase B).
//
// Executor is stateless across waves; per-mission state lives on
// the [*MissionState] handle reachable via state.Value(StateKey).
type Executor struct {
	spawner Spawner
	logger  loggerLike
}

// NewExecutor constructs an Executor backed by spawner. logger may
// be nil; the executor falls back to a no-op logger.
func NewExecutor(spawner Spawner, logger loggerLike) *Executor {
	if logger == nil {
		logger = noopLogger{}
	}
	return &Executor{spawner: spawner, logger: logger}
}

// RunWaveOptions tunes per-wave executor behaviour. Phase A keeps
// the surface minimal — Timeout caps total wave wall-time;
// RenderMode overrides the executor's per-worker default of
// "silent" (rarely needed outside debugging).
type RunWaveOptions struct {
	// Timeout caps the wave's total wall-clock budget. Zero means
	// "use ctx's own deadline / no extra cap".
	Timeout time.Duration

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
	m.BeginWave(wave.Label)

	renderMode := opts.RenderMode
	if renderMode == "" {
		renderMode = "silent"
	}

	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
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

	// Wait for every spawned worker's handoff to land. The
	// observer puts handoffs into m.Handoffs keyed by the
	// canonical ref; the executor polls for ref-presence under a
	// short backoff. Channel-based wakeup would be cleaner — phase
	// B replaces this poll loop with an internal completion channel
	// driven by the observer.
	expected := make([]string, 0, len(wave.Subagents))
	subagentByRef := make(map[string]SubagentSpec, len(wave.Subagents))
	for _, rec := range records {
		if rec.err != nil {
			continue
		}
		ref, _ := MakeRef(rec.spec.Name, wave.Label)
		expected = append(expected, ref)
		subagentByRef[ref] = rec.spec
	}

	if err := waitForRefs(ctx, m.Handoffs, expected); err != nil {
		return WaveStatusFailed, collectOutcomes(records, wave.Label, m.Handoffs), err
	}

	// Aggregate outcomes + status.
	outcomes := collectOutcomes(records, wave.Label, m.Handoffs)
	status := aggregateStatus(outcomes, records)

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

	return status, outcomes, nil
}

// waveRoles returns the role name of every subagent in a wave —
// the input to MissionManifest.TimeoutForRoles so a Do wave's
// wall-clock budget is the MAX of its parallel workers' timeouts.
func waveRoles(w Wave) []string {
	roles := make([]string, 0, len(w.Subagents))
	for _, s := range w.Subagents {
		roles = append(roles, s.Role)
	}
	return roles
}

// waitForRefs polls the store until every ref in refs is present
// or ctx fires. Phase A's simplest possible wakeup; phase B
// replaces it with a completion-channel.
func waitForRefs(ctx context.Context, store *Handoffs, refs []string) error {
	if len(refs) == 0 {
		return nil
	}
	const pollEvery = 50 * time.Millisecond
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	for {
		ready := 0
		for _, r := range refs {
			if _, ok := store.Get(r); ok {
				ready++
			}
		}
		if ready == len(refs) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// spawnRecord is the per-worker bookkeeping the RunWave loop
// builds at spawn-time and re-uses at outcome-collection time.
type spawnRecord struct {
	spec SubagentSpec
	res  SpawnResult
	err  error
}

func collectOutcomes(records []spawnRecord, waveLabel string, store *Handoffs) []DoneWorker {
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
		h, ok := store.Get(ref)
		if !ok {
			entry.Status = "error"
			entry.Error = "no handoff produced before wave ctx expired"
			out = append(out, entry)
			continue
		}
		entry.Status = h.Status
		if h.Status != "ok" {
			entry.Error = h.Reason
		}
		out = append(out, entry)
	}
	return out
}

func aggregateStatus(outcomes []DoneWorker, records []spawnRecord) WaveStatus {
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
