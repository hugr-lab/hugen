package extension

import "context"

// SpawnSpec is the [SessionSpawner] input mirror of
// pkg/session.SpawnSpec. Lives in pkg/extension so external
// extensions can request a child-session spawn without importing
// pkg/session (the import direction is unidirectional:
// pkg/session → pkg/extension).
//
// *session.Session structurally satisfies [SessionSpawner] via a
// translation method that maps these fields onto its own SpawnSpec.
// RenderMode mirrors pkg/protocol.SubagentRender* constants.
type SpawnSpec struct {
	Name       string
	Skill      string
	Role       string
	Task       string
	Inputs     any
	RenderMode string
}

// SessionSpawner is the optional capability a SessionState may
// satisfy when it owns a child-spawn surface — today only
// *session.Session does, structurally. Extensions (the canonical
// caller: mission ext's Plan Executor) type-assert
// `state.(SessionSpawner)` and call SpawnChild. The returned
// SessionState is the child's; ID() / Submit() / etc. work as
// expected.
//
// Returning an error halts the caller's wave / dispatch flow; no
// partial-spawn state is left behind (the underlying Session.Spawn
// is atomic per pkg/session contracts).
type SessionSpawner interface {
	SpawnChild(ctx context.Context, spec SpawnSpec) (SessionState, error)
}

// MissionAutoRunner is the capability the mission ext implements
// to drive a freshly-spawned mission session through its PDCA
// loop without involving the mission's supervisor LLM in the
// kickoff. Called once per mission spawn from
// pkg/session.spawn_mission AFTER the child session is opened,
// AFTER its state-initialisers have run, and BEFORE the first
// user message lands in its inbox.
//
// Contract:
//   - The mission session's [SessionState] is passed in `mission`.
//     Implementations spawn workers from this state via
//     [SessionSpawner].
//   - skill / goal / inputs mirror the spawn_mission args; the
//     implementation looks up the (PDCA-shaped) skill manifest
//     itself via its own catalog handle.
//   - Returning an error surfaces as a `mission_kickoff_failed`
//     tool envelope to the spawning LLM; the mission session is
//     cancelled by the runtime.
//   - The implementation MAY return nil immediately and drive the
//     executor on a goroutine — typical for PDCA where the mission
//     completes asynchronously over many turns.
//
// Extensions other than mission ext do not implement this
// interface in v1; if multiple implementations exist the runtime
// invokes them in registration order and stops at the first
// non-error response. Phase A — one implementer (mission ext).
type MissionAutoRunner interface {
	RunMission(ctx context.Context, mission SessionState, skill, goal string, inputs any) error
}
