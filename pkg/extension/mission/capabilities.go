package mission

// Phase F (design 003) — declarative capability resolver.
//
// The mission-PDCA design ships an opt-in surface for per-role
// capabilities (today: plan_context read) so the implicit "phase
// roles see plan_context, Do roles don't" rule from Phase D
// becomes explicit-or-default-by-role-class. Skills can override
// per role; absent declaration falls through to the role-class
// default.
//
// Resolution order, narrow → broad:
//   1. Per-role override from MissionManifest.Roles[name].
//   2. Role-class default (planner / checker / synthesizer →
//      read; everything else → off).
//
// The resolver is a pure function over the projected manifest —
// no side effects, safe to call from spawn paths without
// locking.

// ResolvePlanContextAccess returns the effective PlanContext
// access mode ("off" | "read") for the worker named `roleName`
// when this is the role within the mission described by
// `manifest`. Empty roleName / nil manifest collapses to the
// role-class default (off).
//
// Phase F: only inspects per-role overrides; the mission-tier
// Capabilities knobs are forward-compat declarative slots and do
// not gate any current runtime behaviour.
func ResolvePlanContextAccess(manifest MissionManifest, roleName string) string {
	if cap, ok := manifest.Roles[roleName]; ok {
		if cap.PlanContextAccess != "" {
			return normalizePlanContextAccess(cap.PlanContextAccess)
		}
	}
	return defaultPlanContextAccessForRole(manifest, roleName)
}

// normalizePlanContextAccess maps user-facing access strings onto
// the canonical enum. Unknown values fall through as "off"
// (defence in depth — manifest validation rejects them at parse
// time so this only fires in tests / programmatic catalog builds).
func normalizePlanContextAccess(s string) string {
	switch s {
	case PlanContextRead:
		return PlanContextRead
	default:
		return PlanContextOff
	}
}

// defaultPlanContextAccessForRole implements the role-class
// rule: phase roles (planner / checker / synthesizer) default
// to `read`; every other role (Do workers) defaults to `off`.
//
// Role-class membership is derived from the manifest's named
// roles — Plan.Role, Control.Role, Synthesis.Role. This keeps
// the resolver decoupled from string identifiers ("planner",
// "checker") that a skill author might choose to name
// differently.
func defaultPlanContextAccessForRole(manifest MissionManifest, roleName string) string {
	if roleName == "" {
		return PlanContextOff
	}
	if manifest.Plan.Role != "" && manifest.Plan.Role == roleName {
		return PlanContextRead
	}
	if manifest.Control.Role != "" && manifest.Control.Role == roleName {
		return PlanContextRead
	}
	if manifest.Synthesis.Role != "" && manifest.Synthesis.Role == roleName {
		return PlanContextRead
	}
	return PlanContextOff
}
