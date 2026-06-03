package mission

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// Extension is the mission-PDCA orchestration extension. It
// composes alongside the existing plan / whiteboard / notepad /
// skill extensions and owns:
//
//   - the per-mission state (PlanState + Handoffs) via
//     [extension.StateInitializer];
//   - the worker terminal-frame parse into the Handoffs store via
//     [extension.ChildFrameObserver];
//   - the liveview state projection via [extension.StatusReporter];
//   - the supervisor / worker tool surfaces under "mission:*" via
//     [tool.ToolProvider].
//
// Mission orchestration NEVER reaches into pkg/session internals.
// Spawning workers uses parent.Spawn(SpawnSpec{RenderMode: ...})
// — the one new field in pkg/session is the
// [protocol.SubagentRenderSilent] plumbing that lets the executor
// suppress per-worker terminal projection on the mission's
// supervisor LLM.
//
// The extension wires StateInitializer + ChildFrameObserver +
// StatusReporter + the mission tools, plus the
// `mission.plan.inline` → executor dispatch path for deterministic
// pipeline missions.
type Extension struct {
	agentID string
	logger  *slog.Logger
	catalog Catalog
}

// Config carries the agent-id stamp used on emitted extension
// frames + a structured logger + the mission catalog mission ext
// reads to validate spawn_mission's `skill` arg. Optional fields
// default to reasonable values; Catalog defaults to an empty
// static catalogue (every skill not mission-eligible).
type Config struct {
	AgentID string
	Logger  *slog.Logger
	Catalog Catalog
}

// NewExtension constructs the mission ext.
func NewExtension(cfg Config) *Extension {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	catalog := cfg.Catalog
	if catalog == nil {
		catalog = NewStaticCatalog()
	}
	return &Extension{
		agentID: cfg.AgentID,
		logger:  logger,
		catalog: catalog,
	}
}

// Compile-time interface assertions — every capability the
// extension claims to satisfy gets a compile-time check so a
// future signature change surfaces here rather than at runtime.
var (
	_ extension.Extension          = (*Extension)(nil)
	_ extension.StateInitializer   = (*Extension)(nil)
	_ extension.ChildFrameObserver = (*Extension)(nil)
	_ extension.StatusReporter     = (*Extension)(nil)
	_ extension.MissionDispatcher  = (*Extension)(nil)
	_ extension.MissionAutoRunner  = (*Extension)(nil)
	_ extension.Advertiser         = (*Extension)(nil)
	_ extension.TurnFinalizeGate   = (*Extension)(nil)
	_ tool.ToolProvider            = (*Extension)(nil)
)

// Name implements [extension.Extension] and [tool.ToolProvider].
func (e *Extension) Name() string { return providerName }

// Lifetime implements [tool.ToolProvider]. State is per-session,
// but the provider value is shared across the agent — matches the
// plan / notepad / whiteboard extensions' pattern.
func (e *Extension) Lifetime() tool.Lifetime { return tool.LifetimePerAgent }

// InitState implements [extension.StateInitializer]. Allocates the
// per-mission [*MissionState] — but ONLY on the mission-tier
// dispatcher session that actually runs the research + Do waves.
//
// A worker / role child (researcher, planner, every Do worker — all
// tier=worker) must reach the MISSION's state (its research output +
// handoff store) via [FromState]'s parent walk. Installing a fresh
// empty MissionState on each of them SHADOWS the mission's: FromState
// finds the worker's own (empty) handle first and never walks up, so
// `mission:get_research` / `mission:get_handoff` always returned
// `available:false` from a worker — the worker then re-discovered the
// schema research had already mapped. The tier check keeps nested
// missions correct: a mission-tier child still owns its own state.
func (e *Extension) InitState(_ context.Context, state extension.SessionState) error {
	if state.Tier() != skill.TierMission {
		if parent, ok := state.Parent(); ok && FromState(parent) != nil {
			// Inherit the nearest ancestor mission's state.
			return nil
		}
	}
	state.SetValue(StateKey, NewMissionState())
	return nil
}

// OnChildFrame implements [extension.ChildFrameObserver]. Parses
// terminal frames from worker children for handoff blocks and
// records them in the mission's Handoffs store.
//
// Recognises three terminal-shaped frames (the same three the
// pump's projectChildFrame promotes to SubagentResult):
//
//  1. *protocol.AgentMessage{Final:true, Consolidated:true} — the
//     worker emitted its final answer at turn-end.
//  2. *protocol.SessionTerminated — fallback projection.
//  3. *protocol.Error — terminal error; recorded as a failed-status
//     handoff so the executor can see "worker errored" without
//     blocking on a non-existent ref.
//
// Other frame kinds (tool_call, reasoning, streaming chunks) are
// dropped — they're the worker's own conversation, not visible
// outputs.
func (e *Extension) OnChildFrame(_ context.Context, parent extension.SessionState, childSessionID string, frame protocol.Frame) {
	m := FromState(parent)
	if m == nil {
		return
	}
	// InquiryRequest frames from any registered child set the
	// inquired-flag for that session id — the planner approval
	// gate reads this post-handoff to verify the planner called
	// session:inquire when policy required it. The flag stays set
	// even after the child closes so a late-arriving bubble is
	// still attributable. Done before the worker/wave gate so
	// workers that inquire mid-turn count, too — useful for the
	// future checker/decider approval flows.
	if _, ok := frame.(*protocol.InquiryRequest); ok {
		m.MarkInquired(childSessionID)
		return
	}
	cur, known := m.LookupWorker(childSessionID)
	if !known {
		return
	}
	wave := m.CurrentWave()
	if wave == "" {
		return
	}
	switch f := frame.(type) {
	case *protocol.AgentMessage:
		if !(f.Payload.Final && f.Payload.Consolidated) {
			return
		}
		// Phase 5.2 — budget-finalize handoff: the runtime stamped this
		// frame (tools were disabled, the role summarised what it had).
		// Record it with the model's summary body but FORCE status:error
		// (context_budget), and flag an orchestration abort. Checked
		// BEFORE the planner special-case so a budget-finalized planner
		// routes to synthesis, not a normal plan completion.
		if f.Payload.BudgetExceeded {
			if isOrchestrationWave(wave) {
				m.MarkBudgetAbort(cur.Role)
			}
			e.ingestHandoff(m, childSessionID, cur, wave, f.Payload.Text, "context_budget", true)
			return
		}
		// Phase 6.x — the planner submits its plan via the single
		// channel mission:validate_and_approve (no terminal ```plan```
		// fence); its final message is just "done". Record the planner
		// wave's completion from the staged submission rather than
		// parsing the (non-fence) text — so a bare "done" doesn't become
		// a parse-error handoff that fails the wave. Other roles
		// (checker / synthesizer / workers) still emit fences.
		if strings.HasPrefix(wave, plannerWaveLabelPrefix) {
			e.ingestPlannerCompletion(m, childSessionID, cur, wave)
			return
		}
		e.ingestHandoff(m, childSessionID, cur, wave, f.Payload.Text, "", false)
	case *protocol.SessionTerminated:
		// Phase 5.2 budget-termination — the child crossed its hard
		// context budget and was force-closed (no handoff: the soft
		// nudge's chance to hand off cleanly was not taken). Checked
		// BEFORE the planner-wave special-case so a budget-terminated
		// planner aborts the mission rather than re-spawning into the
		// same shortfall. Record a clearly-reasoned failed handoff so
		// waitForRefs resolves; for an ORCHESTRATION role flag the
		// mission for an immediate clean abort (a worker just re-plans
		// via the partial wave — its artifact files carry the work).
		if f.Payload.Reason == protocol.TerminationContextBudget {
			e.recordError(m, childSessionID, cur, wave, "context_budget",
				"role hit the context budget; partial work preserved in the mission files")
			if isOrchestrationWave(wave) {
				m.MarkBudgetAbort(cur.Role)
			}
			return
		}
		// Phase 6.x — planner wave: complete from the staged submission
		// (same as the AgentMessage path) regardless of any Result text,
		// so a fence-less planner close still resolves the wave.
		if strings.HasPrefix(wave, plannerWaveLabelPrefix) {
			e.ingestPlannerCompletion(m, childSessionID, cur, wave)
			return
		}
		if f.Payload.Result == "" {
			// The worker closed without any terminal output — no
			// fence to parse (e.g. it exhausted the finalize-gate
			// retries without ever emitting one, or a thinking-model
			// left empty content). Record a FAILED handoff so the
			// executor's waitForRefs resolves and the wave fails
			// cleanly, instead of polling the missing ref forever.
			// (The handoff finalize-gate re-prompts in-session first;
			// this is the backstop for when that cap is exhausted.)
			e.recordError(m, childSessionID, cur, wave, "no_handoff",
				"worker closed without emitting a terminal handoff")
			return
		}
		e.ingestHandoff(m, childSessionID, cur, wave, f.Payload.Result, f.Payload.Reason, false)
	case *protocol.Error:
		e.recordError(m, childSessionID, cur, wave, f.Payload.Code, f.Payload.Message)
	}
}

// isOrchestrationWave reports whether a wave label belongs to a
// runtime-driven orchestration stage (research / planner / checker /
// synthesis) rather than a planner-chosen worker ("Do") wave. Used to
// decide whether a context-budget termination aborts the whole mission
// (orchestration role can't fit → no point retrying) or just re-plans
// (a worker → the next wave continues from its files). Phase 5.2.
func isOrchestrationWave(wave string) bool {
	return strings.HasPrefix(wave, plannerWaveLabelPrefix) ||
		strings.HasPrefix(wave, checkerWaveLabelPrefix) ||
		strings.HasPrefix(wave, researchWaveLabelPrefix) ||
		wave == synthesisWaveLabel
}

// ingestPlannerCompletion records the planner wave's terminal handoff
// from the staged validate_and_approve submission instead of parsing
// a fence (Phase 6.x — the planner has no fence; its final message is
// "done"). The handoff is a completion MARKER for the executor's
// waitForRefs + status aggregation; spawnAndAwaitPlanner reads the
// actual plan from MissionState.PlannerSubmission, not this body.
//
//   - approved or aborted (fresh submission from THIS planner)
//     → OK handoff: the wave succeeds, spawnAndAwaitPlanner branches
//       on approve vs abort.
//   - anything else (never submitted, invalid past the gate cap)
//     → error handoff: the wave fails, the planner loop re-spawns
//       with the gap folded into a synthetic amend verdict.
//
// Idempotent on a duplicate terminal frame (first wins) — matches
// ingestHandoff.
func (e *Extension) ingestPlannerCompletion(m *MissionState, childSessionID string, cur workerCursor, wave string) {
	ref, err := MakeRef(cur.Name, wave)
	if err != nil {
		e.logger.Warn("mission: ingestPlannerCompletion: bad ref",
			"child", childSessionID, "name", cur.Name, "wave", wave, "err", err)
		return
	}
	if _, dup := m.Handoffs.Get(ref); dup {
		return
	}
	subRef := SubagentRef{
		SessionID: childSessionID,
		Name:      cur.Name,
		Role:      cur.Role,
		Skill:     cur.Skill,
	}
	sub := m.PlannerSubmission()
	fresh := sub.called && sub.sessionID == childSessionID
	if fresh && (sub.approved || sub.aborted) {
		m.Handoffs.Put(Handoff{
			Ref:       ref,
			Kind:      KindPlan,
			Status:    "ok",
			Subagent:  subRef,
			CreatedAt: nowFn(),
		})
		return
	}
	reason := "planner closed without submitting an approved plan via mission:validate_and_approve"
	if fresh && sub.called && !sub.valid {
		reason = "planner closed with an invalid plan (validate_and_approve never returned valid:true)"
	}
	m.Handoffs.Put(Handoff{
		Ref:       ref,
		Kind:      KindPlan,
		Status:    "error",
		Reason:    reason,
		Subagent:  subRef,
		CreatedAt: nowFn(),
	})
}

// ingestHandoff is the shared parse+record path. Builds the ref
// from (cur.Name, wave), parses the worker's text, stamps the
// Subagent + Ref fields, stores in Handoffs.
// budgetExceeded (Phase 5.2) is set when the runtime stamped the
// child's finalize handoff frame with BudgetExceeded — the role
// produced this summary under a tools-disabled context-budget cut. We
// keep the model's summary body but FORCE status:error (reason
// context_budget) so an out-of-budget role can never report success.
func (e *Extension) ingestHandoff(m *MissionState, childSessionID string, cur workerCursor, wave, text, fallbackReason string, budgetExceeded bool) {
	ref, err := MakeRef(cur.Name, wave)
	if err != nil {
		e.logger.Warn("mission: ingestHandoff: bad ref",
			"child", childSessionID, "name", cur.Name, "wave", wave, "err", err)
		return
	}
	if _, dup := m.Handoffs.Get(ref); dup {
		// Idempotent on duplicate terminal frames (Phase A keeps it
		// simple — first wins). Phase B's retry path makes this an
		// explicit overwrite-on-retry; for now log and skip.
		e.logger.Debug("mission: ingestHandoff: duplicate ref, skipping",
			"ref", ref, "child", childSessionID)
		return
	}
	h, parseErr := ParseHandoff(text)
	if parseErr != nil {
		// Phase A: a worker that closed without a parseable handoff
		// is recorded as a failed handoff so the executor's wait
		// loop can see it. Phase B replaces this with the
		// output_contract retry pipeline.
		reason := parseErr.Error()
		if fallbackReason != "" {
			reason = fallbackReason + ": " + reason
		}
		m.Handoffs.Put(Handoff{
			Ref:    ref,
			Kind:   KindHandoff,
			Status: "error",
			Reason: reason,
			// Preserve the role's raw output as the body so the planner
			// can mission:get_handoff(ref) to see WHAT IT ACCOMPLISHED —
			// even when the summary didn't parse as a clean fenced
			// handoff (e.g. a budget-cut role's free-text wrap-up).
			Body: text,
			Subagent: SubagentRef{
				SessionID: childSessionID,
				Name:      cur.Name,
				Role:      cur.Role,
				Skill:     cur.Skill,
			},
			CreatedAt: nowFn(),
		})
		return
	}
	h.Ref = ref
	h.Subagent = SubagentRef{
		SessionID: childSessionID,
		Name:      cur.Name,
		Role:      cur.Role,
		Skill:     cur.Skill,
	}
	h.CreatedAt = nowFn()
	// Phase 5.2 — the runtime caught the context budget: keep the
	// model's summary body but FORCE status:error so the role cannot
	// claim success on an out-of-budget, tools-disabled finalize.
	if budgetExceeded {
		h.Status = "error"
		if h.Reason == "" {
			h.Reason = "context_budget: ran out of context budget — output is incomplete"
		} else {
			h.Reason = "context_budget: " + h.Reason
		}
	}
	m.Handoffs.Put(h)
	// Phase D — auto-extract memory_summary into the plan_context
	// journal. AppendHandoff is a no-op when MemorySummary is
	// empty, so handoffs from skills that don't write summaries
	// silently pass through.
	m.mu.Lock()
	iter := m.IterationCounter
	m.mu.Unlock()
	m.PlanContext.AppendHandoff(iter, wave, h)
	// Phase 5.x — B11 §3.3. Worker satisfies channel. Each id in
	// h.Satisfies marks the row satisfied with synthesised evidence
	// "worker {role} handoff iter-N wave-{label}". Best-effort —
	// unknown ids logged and skipped (workers can't always know the
	// canonical id roster, and we don't want a typo to fail the
	// wave). Already-satisfied rows are left untouched so worker
	// claims can't overwrite checker-confirmed evidence.
	if len(h.Satisfies) > 0 {
		applied, unknown := m.ApplyWorkerSatisfies(h.Satisfies, iter, cur.Role, wave)
		if len(unknown) > 0 {
			e.logger.Debug("mission: ingestHandoff: worker satisfies referenced unknown ids",
				"ref", ref, "role", cur.Role, "unknown", unknown)
		}
		if len(applied) > 0 {
			e.logger.Debug("mission: ingestHandoff: worker satisfies applied",
				"ref", ref, "role", cur.Role, "applied", applied)
		}
	}
	// Phase 5.x — B13. Skill-driven approval invalidation. Any
	// worker (regardless of role) can request that the next planner
	// iteration re-open the approval modal by including
	// `invalidates_plan_approval: true` in its handoff body — and
	// optionally a short `invalidates_reason` explaining what
	// changed. The runtime carries pendingReapproval until the next
	// modal closes approve. Skill prose decides which roles wave
	// this flag — runtime stays role-agnostic.
	if invalidates, reason := invalidatesPlanApproval(h.Body); invalidates {
		m.RequestReapproval(reason)
		e.logger.Info("mission: ingestHandoff: worker invalidated plan approval",
			"ref", ref, "child", childSessionID, "role", cur.Role, "reason", reason)
	}
}

// invalidatesPlanApproval extracts the `invalidates_plan_approval`
// flag and its optional `invalidates_reason` companion from a
// handoff body. Returns (true, reason) only when the body is a map
// whose `invalidates_plan_approval` parses as boolean true; the
// reason is best-effort (empty string when missing or not a
// string). Default-safe: any non-map / missing / non-bool input
// yields (false, "") — invalidation is opt-in.
func invalidatesPlanApproval(body any) (bool, string) {
	m, ok := body.(map[string]any)
	if !ok {
		return false, ""
	}
	v, ok := m["invalidates_plan_approval"]
	if !ok {
		return false, ""
	}
	b, ok := v.(bool)
	if !ok || !b {
		return false, ""
	}
	reason, _ := m["invalidates_reason"].(string)
	return true, reason
}

// recordError stores a synthetic error handoff for a worker that
// terminated with an Error frame.
func (e *Extension) recordError(m *MissionState, childSessionID string, cur workerCursor, wave, code, message string) {
	ref, err := MakeRef(cur.Name, wave)
	if err != nil {
		return
	}
	if _, dup := m.Handoffs.Get(ref); dup {
		return
	}
	m.Handoffs.Put(Handoff{
		Ref:    ref,
		Kind:   KindHandoff,
		Status: "error",
		Reason: code + ": " + message,
		Subagent: SubagentRef{
			SessionID: childSessionID,
			Name:      cur.Name,
			Role:      cur.Role,
			Skill:     cur.Skill,
		},
		CreatedAt: nowFn(),
	})
}

// ReportStatus implements [extension.StatusReporter]. Returns the
// per-mission projection (PlanState + handoff count) as opaque
// JSON for liveview to fold into the SessionStatusPayload.
//
// Returns nil when the session has no MissionState handle
// (non-mission sessions stay invisible to liveview's mission
// surface).
func (e *Extension) ReportStatus(_ context.Context, state extension.SessionState) json.RawMessage {
	v, ok := state.Value(StateKey)
	if !ok {
		return nil
	}
	m, _ := v.(*MissionState)
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.currentWave == "" && len(m.Plan.Done) == 0 {
		return nil
	}
	payload := struct {
		Plan         PlanState `json:"plan"`
		ActiveWave   string    `json:"active_wave,omitempty"`
		HandoffCount int       `json:"handoff_count"`
	}{
		Plan:         m.Plan,
		ActiveWave:   m.currentWave,
		HandoffCount: m.Handoffs.Len(),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return data
}
