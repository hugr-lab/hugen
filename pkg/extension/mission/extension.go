package mission

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
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
// Phase A — only StateInitializer + ChildFrameObserver +
// StatusReporter + the two tools are wired. The executor exists as
// a callable primitive but no automatic dispatch path triggers it;
// the integration scenario / a future phase wires the
// `mission.plan.experimental_inline` → executor link.
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
	_ tool.ToolProvider            = (*Extension)(nil)
)

// Name implements [extension.Extension] and [tool.ToolProvider].
func (e *Extension) Name() string { return providerName }

// Lifetime implements [tool.ToolProvider]. State is per-session,
// but the provider value is shared across the agent — matches the
// plan / notepad / whiteboard extensions' pattern.
func (e *Extension) Lifetime() tool.Lifetime { return tool.LifetimePerAgent }

// InitState implements [extension.StateInitializer]. Allocates a
// fresh [*MissionState] handle for the calling session. Phase A
// allocates for every session; future phases (F) may gate on
// "mission-eligible" sessions only to keep the state map empty on
// pure root chats — for now the zero-state handle is cheap.
func (e *Extension) InitState(_ context.Context, state extension.SessionState) error {
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
		e.ingestHandoff(m, childSessionID, cur, wave, f.Payload.Text, "")
	case *protocol.SessionTerminated:
		if f.Payload.Result == "" {
			// Nothing to parse — leave the cursor open for now.
			// Phase A keeps it minimal: a worker that closes
			// without a Result and without a prior final
			// AgentMessage is treated as "no handoff produced"
			// and the executor will time out / move on.
			return
		}
		e.ingestHandoff(m, childSessionID, cur, wave, f.Payload.Result, f.Payload.Reason)
	case *protocol.Error:
		e.recordError(m, childSessionID, cur, wave, f.Payload.Code, f.Payload.Message)
	}
}

// ingestHandoff is the shared parse+record path. Builds the ref
// from (cur.Name, wave), parses the worker's text, stamps the
// Subagent + Ref fields, stores in Handoffs.
func (e *Extension) ingestHandoff(m *MissionState, childSessionID string, cur workerCursor, wave, text, fallbackReason string) {
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
	m.Handoffs.Put(h)
	// Phase D — auto-extract memory_summary into the plan_context
	// journal. AppendHandoff is a no-op when MemorySummary is
	// empty, so handoffs from skills that don't write summaries
	// silently pass through.
	m.mu.Lock()
	iter := m.IterationCounter
	m.mu.Unlock()
	m.PlanContext.AppendHandoff(iter, wave, h)
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
