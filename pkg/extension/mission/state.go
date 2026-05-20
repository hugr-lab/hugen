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

	// currentApprovedMarker is the canonical sha256-hex of the plan
	// body the user most recently approved (Phase I.23). Empty when
	// no plan is currently approved — either because no inquire has
	// landed yet, or because a downstream worker invalidated the
	// approval via its handoff body's `invalidates_plan_approval`
	// flag. mission:validate_and_approve checks against this:
	//
	//   - marker(body) == currentApprovedMarker → already approved,
	//     return idempotently without re-inquiring (the planner
	//     re-emitted the SAME body — a refine loop converging).
	//   - mismatch / empty → run the inquire; on approve overwrite
	//     with the new marker.
	//
	// spawnAndAwaitPlanner's gate uses the same value to verify the
	// planner emits the SAME body it had approved.
	//
	// Skill-agnostic by design: the runtime knows nothing about
	// role names; it only knows "approved-or-not". Skills express
	// their own per-role invalidation policy via the
	// `invalidates_plan_approval` handoff field — clarification /
	// scoping roles typically set it true because their findings
	// reshape what the next planner should propose, while execution
	// roles leave it false so the approved plan flows through.
	currentApprovedMarker string

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

	// currentMissionAC is the latest planner's list of mission
	// acceptance criteria. Snapshotted from
	// Plan.MissionAcceptanceCriteria after spawnAndAwaitPlanner
	// accepts a planner handoff. Surfaced in the checker's task as
	// `[Mission acceptance criteria]`; the checker emits per-
	// criterion satisfied flags in its verdict body. Phase I.26.
	currentMissionAC []string

	// currentWaveAC tracks the acceptance criteria of the wave
	// currently in flight (or just completed). Set by
	// spawnAndAwaitPlanner from Plan.NextWave.AcceptanceCriteria
	// when accepting the plan, surfaced to the checker as `[Wave
	// acceptance criteria]`. Cleared on wave boundary. Optional
	// per the plan shape — empty when the planner didn't narrow.
	// Phase I.26.
	currentWaveAC []string
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

// SetApprovedPlanMarker overwrites the mission's current approved
// plan marker. Empty input is treated as "clear approval". Called
// by the validate_and_approve tool after the user's approve reply.
func (m *MissionState) SetApprovedPlanMarker(marker string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.currentApprovedMarker = marker
}

// ApprovedPlanMarker returns the mission's currently approved plan
// marker, or "" when no plan is approved (either no inquire has
// landed yet, or a worker invalidated the prior approval).
func (m *MissionState) ApprovedPlanMarker() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.currentApprovedMarker
}

// InvalidatePlanApproval clears the mission's currently approved
// plan marker. Called by ingestHandoff when a worker emits a
// handoff body carrying `invalidates_plan_approval: true` — the
// worker's discovery (e.g. user-clarified inputs) means the next
// planner spawn cannot rely on the prior approval and must
// re-validate from scratch.
//
// Skill-agnostic: the runtime decides "the next plan needs fresh
// approval"; per-skill prose decides which roles set the flag.
func (m *MissionState) InvalidatePlanApproval() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.currentApprovedMarker = ""
}

// SetMissionFrame stamps the planner's current understanding of
// what the mission delivers + the exit criteria the checker reads.
// Called by spawnAndAwaitPlanner when accepting a non-complete
// plan; latest wins (each iteration's planner re-emits the full
// list, runtime keeps the snapshot). Phase I.26.
func (m *MissionState) SetMissionFrame(goal string, mission []string, wave []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.currentMissionGoal = strings.TrimSpace(goal)
	m.currentMissionAC = append(m.currentMissionAC[:0:0], mission...)
	m.currentWaveAC = append(m.currentWaveAC[:0:0], wave...)
}

// MissionFrame reads the latest planner-set frame. Returns empty
// values when no plan has been accepted yet. Phase I.26.
func (m *MissionState) MissionFrame() (goal string, mission []string, wave []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mission = append([]string(nil), m.currentMissionAC...)
	wave = append([]string(nil), m.currentWaveAC...)
	return m.currentMissionGoal, mission, wave
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
