package mission

import (
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

	Plan      PlanState
	Handoffs  *Handoffs

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
