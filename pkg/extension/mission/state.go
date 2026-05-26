package mission

import (
	"strings"
	"sync"

	"github.com/hugr-lab/hugen/pkg/extension"
)

// StateKey is the [extension.SessionState] key the extension stores
// its per-session [*MissionState] handle under. Exported so other
// packages can recover the handle when wiring auxiliary surfaces
// (status renderers, scenario harness assertions).
const StateKey = "mission"

// providerName doubles as the tool prefix ("mission:<tool>") and
// the extension's [Extension.Name].
const providerName = "mission"

// MissionState is the per-session typed handle the mission ext
// stores in [extension.SessionState]. Carries the three core
// projections every mission session owns:
//
//   - Plan: the PlanState projection (Done waves, Active wave,
//     Roadmap, Iteration counter).
//   - Handoffs: the keyed store of every worker / phase-role
//     handoff produced under this mission.
//   - currentWave: the wave label the executor is currently
//     filling. Used by ChildFrameObserver to assign refs.
//
// Per-mission state lives only on the mission session — not on
// workers. Worker sessions get a MissionState handle for the
// `mission:get_handoff` tool that points into the mission's
// Handoffs by following Parent() chain (see FromState).
type MissionState struct {
	mu sync.Mutex

	Plan        PlanState
	Handoffs    *Handoffs
	PlanContext *PlanContext

	// IterationCounter mirrors the planner-loop's current
	// iteration index. Used by ingestHandoff so plan_context
	// entries auto-tag with the right iteration. Updated by the
	// planner loop on every iteration_start emit.
	IterationCounter int

	// currentWave names the wave the executor is currently filling.
	// Empty between waves. ChildFrameObserver reads this to assign
	// refs to incoming handoffs.
	currentWave string

	// workersInWave tracks which spawned children belong to the
	// active wave. Keyed by child session id, value is the worker's
	// configured name (the ref's left-hand side).
	workersInWave map[string]workerCursor

	// inquired tracks per-child session ids that have observed at
	// least one *protocol.InquiryRequest frame bubble through the
	// mission's ChildFrameObserver. The planner approval gate (γ)
	// reads this map post-handoff to verify the planner actually
	// asked for approval when policy required it. Entries persist
	// across waves so a late-arriving frame on a closed planner
	// session is still attributable.
	inquired map[string]bool

	// firstPlanApproved flips to true the first time the user closes
	// the approval modal with approve=true (or when the policy opts
	// out via Initial=skip — the implicit-approve path also flips
	// it). Phase 5.x — B13. The gate uses this bit + pendingReapproval
	// + the planner's RequiresReapproval flag to decide whether to
	// (re-)open the modal. Never reset within a mission.
	firstPlanApproved bool

	// pendingReapproval is set when a worker handoff carried
	// `invalidates_plan_approval: true` since the last modal closed
	// approve. The next planner iteration's validate_and_approve
	// call re-opens the modal regardless of the planner's own
	// RequiresReapproval flag. Cleared once the modal closes
	// approve. pendingReapprovalReason carries the worker's
	// `invalidates_reason` (when present) so the planner / modal
	// can surface why approval was invalidated.
	pendingReapproval       bool
	pendingReapprovalReason string

	// plannerApproval mirrors MissionManifest.Plan.Approval so the
	// validate_and_approve tool handler (which doesn't see the full
	// manifest) can apply the same approval-required predicate as
	// the runtime's post-close gate. Stamped by the auto-runner at
	// RunMission time.
	plannerApproval PlanApproval

	// currentMissionGoal is the latest planner's restatement of what
	// the mission delivers. Snapshotted from Plan.MissionGoal after
	// spawnAndAwaitPlanner accepts a planner handoff. Surfaced in
	// the checker's task as `[Mission goal (planner's framing)]`.
	// Phase I.26.
	currentMissionGoal string

	// ac is the identity-bearing list of acceptance criteria — the
	// single source of truth for what the mission must deliver. Each
	// row carries id / statement / origin / status / evidence + iter
	// stamps. Mutated only via the helpers in state_ac.go (SeedAC,
	// CommitStagedDiff, ApplyStatusOnly, ApplyWorkerSatisfies); never
	// touch the slice directly outside that file.
	//
	// Phase 5.x — B11.
	ac []AcceptanceCriterion

	// nextACID is the monotonically-increasing id counter for new
	// rows. Runtime stamps "ac-<nextACID>" on every ac_add then
	// increments. IDs are never reused inside a mission — even
	// dropped rows keep theirs for audit.
	nextACID int

	// pendingDiff is the staged planner diff awaiting approval. Set
	// by StagePlannerDiff when the planner emits a contract-changing
	// or requires_reapproval iter; cleared by CommitStagedDiff
	// (modal-approve / refine) or DiscardStagedDiff (modal-reject).
	// Nil between approval gates.
	//
	// Phase 5.x — B11 §3.2.1.
	pendingDiff *stagedDiff

	// currentWaveAC tracks the acceptance criteria of the wave
	// currently in flight (or just completed). Set by
	// spawnAndAwaitPlanner from Plan.NextWave.AcceptanceCriteria
	// when accepting the plan, surfaced to the checker as `[Wave
	// acceptance criteria]`. Cleared on wave boundary. Optional
	// per the plan shape — empty when the planner didn't narrow.
	// Phase I.26.
	currentWaveAC []string

	// researchFindings is the free-form summary the research role
	// emitted on its done=true handoff. Surfaced to the planner via
	// plan_context.research_findings so iter-1 plans see what the
	// research stage discovered. Phase 5.x — B15.
	researchFindings string

	// resolvedUserInputs is the structured key/value map the
	// research role surfaced via its done=true handoff. Planner
	// reads it under plan_context.resolved_user_inputs (treats
	// each entry as a user-confirmed input for the downstream
	// workers). Phase 5.x — B15.
	resolvedUserInputs map[string]any

	// researchACProposals is the per-criterion list the research
	// role recommended for the planner to consider. Planner is
	// the authority on what becomes mission_acceptance_criteria;
	// proposals are input only (§3.2.1). Phase 5.x — B15.
	researchACProposals []ResearchACProposal

	// autoApproveTools mirrors the user's last pick on an approval
	// modal — true when they chose "approve with tools", false on
	// "approve" / refine / abort / reject. RESET to false at the
	// top of every fresh approval modal (before RequestInquiry
	// returns) so each modal asks afresh; the flag does NOT
	// auto-inherit across replans. Consulted by MaybeAutoApprove
	// (§4.6.5) on every requires_approval tool call — when set on
	// any ancestor mission in the caller's parent chain, the tool
	// inquiry is skipped and approval granted immediately.
	//
	// Phase 5.x — §4.6.
	autoApproveTools bool

	// researchAttempted tracks whether the research stage was
	// invoked on this mission, regardless of outcome. Flipped to
	// true at the very start of runResearchStage when the manifest
	// declares a research block AND the When-predicate fires.
	// Read by callGetResearch to disambiguate three otherwise-
	// indistinguishable "available: false" cases:
	//   - manifest had no research block          → !attempted, no findings
	//   - research ran but emitted empty findings → attempted, no findings
	//   - research ran and was aborted            → attempted, no findings
	// `available: false, attempted: true` signals to the worker
	// that research was tried but yielded nothing — the worker
	// should NOT assume scope was researched.
	researchAttempted bool
}

// workerCursor names a spawned worker so ChildFrameObserver can
// build the handoff Ref ("<Name>@<currentWave>") when the worker's
// terminal frame arrives.
type workerCursor struct {
	Name  string
	Role  string
	Skill string
}

// NewMissionState constructs a zero-value MissionState with an
// empty Handoffs store. Used by [Extension.InitState].
func NewMissionState() *MissionState {
	return &MissionState{
		Handoffs:      NewHandoffs(),
		PlanContext:   NewPlanContext(),
		workersInWave: make(map[string]workerCursor),
		inquired:      make(map[string]bool),
	}
}

// CurrentWave reports the wave label the executor is filling, or
// "" if no wave is active.
func (m *MissionState) CurrentWave() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.currentWave
}

// BeginWave marks wave as currently active and clears the
// per-wave worker tracking. Called by the executor at the top of
// RunWave.
func (m *MissionState) BeginWave(label string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.currentWave = label
	m.workersInWave = make(map[string]workerCursor)
}

// RegisterWorker records the (sessionID → cursor) mapping the
// observer will later look up. Called by the executor right after
// each Spawn returns.
func (m *MissionState) RegisterWorker(sessionID string, cur workerCursor) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.workersInWave == nil {
		m.workersInWave = make(map[string]workerCursor)
	}
	m.workersInWave[sessionID] = cur
}

// LookupWorker returns the cursor for sessionID, or zero+false
// when the id is unknown (frame from a non-mission child, or a
// stale frame after wave switch).
func (m *MissionState) LookupWorker(sessionID string) (workerCursor, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.workersInWave[sessionID]
	return cur, ok
}

// MarkInquired records that the child at sessionID emitted at
// least one *protocol.InquiryRequest frame. Idempotent — repeated
// inquiry frames on the same session collapse to a single bit.
// Called from [Extension.OnChildFrame] for the planner approval
// gate.
func (m *MissionState) MarkInquired(sessionID string) {
	if sessionID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inquired == nil {
		m.inquired = make(map[string]bool)
	}
	m.inquired[sessionID] = true
}

// Inquired reports whether sessionID has been marked via
// [MarkInquired] at any point in this mission's lifetime. Used by
// the planner approval gate to verify the planner called
// session:inquire when policy required it.
func (m *MissionState) Inquired(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inquired[sessionID]
}

// SetPlannerApproval stamps the dispatching mission's approval
// policy on the mission state so the validate_and_approve tool can
// branch on it without seeing the full MissionManifest. Idempotent;
// the auto-runner calls it once at RunMission time.
func (m *MissionState) SetPlannerApproval(policy PlanApproval) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.plannerApproval = policy
}

// PlannerApproval returns the stamped approval policy, zero-valued
// when SetPlannerApproval never fired.
func (m *MissionState) PlannerApproval() PlanApproval {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.plannerApproval
}

// MarkPlanApproved flips firstPlanApproved on and clears any
// pending reapproval request. Called by validate_and_approve after
// the user's approve reply (and by the implicit-approve path when
// the mission's policy opts out of approvals entirely). Idempotent.
// Phase 5.x — B13.
func (m *MissionState) MarkPlanApproved() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.firstPlanApproved = true
	m.pendingReapproval = false
	m.pendingReapprovalReason = ""
}

// IsPlanApproved reports whether the user has ever approved a plan
// in this mission. Used by validate_and_approve to skip the modal
// on subsequent iterations when nothing has invalidated the prior
// approval, and by spawnAndAwaitPlanner to require approval before
// accepting a plan handoff. Phase 5.x — B13.
func (m *MissionState) IsPlanApproved() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.firstPlanApproved
}

// RequestReapproval marks the mission as needing a fresh approval
// modal on the next planner iteration. Called by ingestHandoff
// when a worker emits a handoff body carrying
// `invalidates_plan_approval: true`. The reason (free-form short
// string from the worker, optional) surfaces in the modal so the
// user sees what changed. Skill-agnostic: the runtime decides "the
// next plan needs fresh approval"; per-skill prose decides which
// roles set the flag. Phase 5.x — B13.
func (m *MissionState) RequestReapproval(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pendingReapproval = true
	m.pendingReapprovalReason = strings.TrimSpace(reason)
}

// PendingReapproval returns (true, reason) when a worker handoff
// has invalidated the prior approval since the last modal closed
// approve. The next planner iteration's validate_and_approve call
// must re-open the modal. Phase 5.x — B13.
func (m *MissionState) PendingReapproval() (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pendingReapproval, m.pendingReapprovalReason
}

// SetGoalAndWaveAC stamps the planner's restated mission_goal and
// the per-wave acceptance criteria. The MISSION-level AC list is no
// longer carried as a flat string slice — it now lives on state.AC
// (the structured B11 model) and is mutated through StagePlannerDiff
// / CommitStagedDiff / ApplyStatusOnly / ApplyWorkerSatisfies.
//
// Empty mission goal is accepted (matches the prior SetMissionFrame
// behaviour, which trimmed whitespace and stored regardless). Wave
// AC may be empty when the planner didn't narrow.
//
// Phase 5.x — B11 §3.2.
func (m *MissionState) SetGoalAndWaveAC(goal string, wave []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.currentMissionGoal = strings.TrimSpace(goal)
	m.currentWaveAC = append(m.currentWaveAC[:0:0], wave...)
}

// MissionFrame returns (goal, mission-AC statements, wave-AC). The
// mission-AC slice is a fresh projection of state.AC: rows whose
// status != dropped, in insertion order, with statement-only
// rendering. Empty list when no AC seeded yet. Used by the checker
// task template + approval modal renderer for the legacy
// string-bullet view while the structured ac_view (ζ) lands.
//
// Phase 5.x — B11 §3.2 (was Phase I.26's SetMissionFrame snapshot).
func (m *MissionState) MissionFrame() (goal string, mission []string, wave []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	wave = append([]string(nil), m.currentWaveAC...)
	mission = make([]string, 0, len(m.ac))
	for _, row := range m.ac {
		if row.Status == ACDropped {
			continue
		}
		mission = append(mission, row.Statement)
	}
	return m.currentMissionGoal, mission, wave
}

// SetAutoApproveTools sets the auto-approve-tools flag. Called by
// validate_and_approve after a successful approve-with-tools modal
// (value=true) and at the top of every fresh approval modal
// (value=false, the reset) so the user's pick from a prior modal
// doesn't silently carry over into a replan. Phase 5.x — §4.6.
func (m *MissionState) SetAutoApproveTools(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.autoApproveTools = v
}

// AutoApproveTools reports the current auto-approve-tools flag.
// MaybeAutoApprove walks the caller's parent chain looking for an
// ancestor mission with this flag set; on hit it skips the tool
// approval inquiry entirely. Phase 5.x — §4.6.
func (m *MissionState) AutoApproveTools() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.autoApproveTools
}

// SetResearchOutput stashes the research role's done=true result
// on mission state so the subsequent planner spawn reads it from
// plan_context. Idempotent (subsequent calls overwrite). Phase
// 5.x — B15.
func (m *MissionState) SetResearchOutput(findings string, resolvedUserInputs map[string]any, acProposals []ResearchACProposal) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.researchFindings = strings.TrimSpace(findings)
	if len(resolvedUserInputs) == 0 {
		m.resolvedUserInputs = nil
	} else {
		m.resolvedUserInputs = make(map[string]any, len(resolvedUserInputs))
		for k, v := range resolvedUserInputs {
			m.resolvedUserInputs[k] = v
		}
	}
	if len(acProposals) == 0 {
		m.researchACProposals = nil
	} else {
		m.researchACProposals = append(m.researchACProposals[:0:0], acProposals...)
	}
}

// MarkResearchAttempted flips the researchAttempted bit on.
// Called by runResearchStage at the start of the loop AFTER the
// When-predicate fires so callGetResearch can disambiguate "no
// research configured" from "research ran but failed / aborted".
// Phase 5.x — B15 follow-up.
func (m *MissionState) MarkResearchAttempted() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.researchAttempted = true
}

// ResearchAttempted reports whether the research stage was
// invoked on this mission, regardless of outcome.
func (m *MissionState) ResearchAttempted() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.researchAttempted
}

// ResearchOutput returns the stashed research output (findings +
// resolved user inputs + ac proposals). Empty / nil when the
// mission has no research stage or it hasn't yet emitted done=true.
// Phase 5.x — B15.
func (m *MissionState) ResearchOutput() (findings string, resolvedUserInputs map[string]any, acProposals []ResearchACProposal) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.resolvedUserInputs) > 0 {
		resolvedUserInputs = make(map[string]any, len(m.resolvedUserInputs))
		for k, v := range m.resolvedUserInputs {
			resolvedUserInputs[k] = v
		}
	}
	if len(m.researchACProposals) > 0 {
		acProposals = append(acProposals, m.researchACProposals...)
	}
	return m.researchFindings, resolvedUserInputs, acProposals
}

// FromState resolves the [*MissionState] handle attached to state,
// or nil if the extension's StateInitializer never ran for it
// (root sessions, non-mission workers).
//
// For workers (children of a mission session), FromState walks
// state.Parent() to find the mission session whose state carries
// the handle — this is what makes mission:get_handoff work from
// inside a worker's tool dispatch context.
func FromState(state extension.SessionState) *MissionState {
	if state == nil {
		return nil
	}
	if v, ok := state.Value(StateKey); ok {
		if m, _ := v.(*MissionState); m != nil {
			return m
		}
	}
	if parent, ok := state.Parent(); ok {
		return FromState(parent)
	}
	return nil
}
