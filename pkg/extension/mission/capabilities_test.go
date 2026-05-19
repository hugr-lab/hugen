package mission

import "testing"

// TestResolvePlanContextAccess_RoleClassDefaults covers the Phase F
// default rule: phase roles (planner / checker / synthesizer)
// default to `read`; every other role defaults to `off`.
func TestResolvePlanContextAccess_RoleClassDefaults(t *testing.T) {
	manifest := MissionManifest{
		Name: "test-skill",
		Plan: MissionPlanManifest{
			Role: "planner-role",
		},
		Control: ControlManifest{
			Role: "checker-role",
		},
		Synthesis: SynthesisManifest{
			Role: "synth-role",
		},
	}

	cases := []struct {
		name string
		role string
		want string
	}{
		{"planner phase role → read", "planner-role", PlanContextRead},
		{"checker phase role → read", "checker-role", PlanContextRead},
		{"synthesizer phase role → read", "synth-role", PlanContextRead},
		{"unknown Do role → off", "echo-worker", PlanContextOff},
		{"empty role → off", "", PlanContextOff},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolvePlanContextAccess(manifest, tc.role)
			if got != tc.want {
				t.Errorf("ResolvePlanContextAccess(%q) = %q, want %q",
					tc.role, got, tc.want)
			}
		})
	}
}

// TestResolvePlanContextAccess_PerRoleOverride covers explicit
// per-role overrides — manifest-declared `capabilities.plan_context`
// wins over the role-class default for both phase roles and Do
// roles.
func TestResolvePlanContextAccess_PerRoleOverride(t *testing.T) {
	manifest := MissionManifest{
		Plan: MissionPlanManifest{Role: "planner-role"},
		Roles: map[string]RoleCapabilities{
			// Phase role explicitly opted out — should be off.
			"planner-role": {PlanContextAccess: PlanContextOff},
			// Do role opted in — should be read.
			"echo-worker": {PlanContextAccess: PlanContextRead},
		},
	}

	cases := []struct {
		name string
		role string
		want string
	}{
		{"phase role explicit off overrides default-read", "planner-role", PlanContextOff},
		{"Do role explicit read overrides default-off", "echo-worker", PlanContextRead},
		{"role with no override still uses default", "other-worker", PlanContextOff},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolvePlanContextAccess(manifest, tc.role)
			if got != tc.want {
				t.Errorf("ResolvePlanContextAccess(%q) = %q, want %q",
					tc.role, got, tc.want)
			}
		})
	}
}

// TestResolvePlanContextAccess_UnknownValue covers the
// defence-in-depth path: an unknown access mode that bypassed
// manifest validation (e.g. programmatic catalog build) falls back
// to `off`.
func TestResolvePlanContextAccess_UnknownValue(t *testing.T) {
	manifest := MissionManifest{
		Roles: map[string]RoleCapabilities{
			"worker": {PlanContextAccess: "write"}, // invalid; not "off"/"read".
		},
	}
	got := ResolvePlanContextAccess(manifest, "worker")
	if got != PlanContextOff {
		t.Errorf("ResolvePlanContextAccess(unknown access) = %q, want %q (defence-in-depth)",
			got, PlanContextOff)
	}
}
