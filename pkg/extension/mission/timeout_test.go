package mission

import (
	"testing"
	"time"
)

// TestTimeoutForRole covers the single-role resolution: an explicit
// per-role timeout wins; an unset or unknown role falls back to
// DefaultWaveTimeout.
func TestTimeoutForRole(t *testing.T) {
	m := MissionManifest{
		Roles: map[string]RoleCapabilities{
			"planner": {Timeout: 15 * time.Minute},
			"checker": {Timeout: 20 * time.Minute},
			"noop":    {PlanContextAccess: "read"}, // declared, but no timeout
		},
	}
	if got := m.TimeoutForRole("planner"); got != 15*time.Minute {
		t.Errorf("planner = %v, want 15m", got)
	}
	if got := m.TimeoutForRole("checker"); got != 20*time.Minute {
		t.Errorf("checker = %v, want 20m", got)
	}
	// Role present but Timeout==0 → default.
	if got := m.TimeoutForRole("noop"); got != DefaultWaveTimeout {
		t.Errorf("noop = %v, want DefaultWaveTimeout", got)
	}
	// Unknown role → default.
	if got := m.TimeoutForRole("ghost"); got != DefaultWaveTimeout {
		t.Errorf("ghost = %v, want DefaultWaveTimeout", got)
	}
}

// TestTimeoutForRoles covers a Do wave's budget = MAX of its parallel
// workers' timeouts, with the default fallback when none declare one.
func TestTimeoutForRoles(t *testing.T) {
	m := MissionManifest{
		Roles: map[string]RoleCapabilities{
			"data-analyst":   {Timeout: 1 * time.Hour},
			"report-builder": {Timeout: 30 * time.Minute},
		},
	}
	// Max across the wave's roles.
	if got := m.TimeoutForRoles([]string{"report-builder", "data-analyst"}); got != 1*time.Hour {
		t.Errorf("max = %v, want 1h", got)
	}
	// Single role.
	if got := m.TimeoutForRoles([]string{"report-builder"}); got != 30*time.Minute {
		t.Errorf("single = %v, want 30m", got)
	}
	// No role declares a timeout → default.
	if got := m.TimeoutForRoles([]string{"unknown-a", "unknown-b"}); got != DefaultWaveTimeout {
		t.Errorf("none declared = %v, want DefaultWaveTimeout", got)
	}
	// Empty wave → default.
	if got := m.TimeoutForRoles(nil); got != DefaultWaveTimeout {
		t.Errorf("empty = %v, want DefaultWaveTimeout", got)
	}
}

// TestWaveRoles extracts each subagent's role for the timeout max.
func TestWaveRoles(t *testing.T) {
	w := Wave{Subagents: []SubagentSpec{
		{Name: "a", Role: "data-analyst"},
		{Name: "b", Role: "report-builder"},
	}}
	got := waveRoles(w)
	if len(got) != 2 || got[0] != "data-analyst" || got[1] != "report-builder" {
		t.Errorf("waveRoles = %v, want [data-analyst report-builder]", got)
	}
}
