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
