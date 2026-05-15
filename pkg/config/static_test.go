package config

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestNewStaticService_DefaultRefreshInterval(t *testing.T) {
	s := NewStaticService(StaticInput{})
	if got := s.RefreshInterval(); got != 5*time.Minute {
		t.Fatalf("default refresh interval = %v, want 5m", got)
	}
	if got := s.permSettings.HardExpiry; got != 15*time.Minute {
		t.Fatalf("default hard expiry = %v, want 15m (3× refresh)", got)
	}
}

func TestNewStaticService_RefreshIntervalRespected(t *testing.T) {
	s := NewStaticService(StaticInput{
		PermSettings: PermissionSettings{
			RefreshInterval: 30 * time.Second,
			RemoteEnabled:   true,
		},
	})
	if got := s.RefreshInterval(); got != 30*time.Second {
		t.Fatalf("refresh interval = %v, want 30s", got)
	}
	if !s.RemoteEnabled() {
		t.Fatal("RemoteEnabled() = false, want true")
	}
}

func TestStaticService_LocalView(t *testing.T) {
	s := NewStaticService(StaticInput{
		LocalDB:        LocalConfig{DB: DBConfig{Path: "/tmp/test.db"}},
		LocalDBEnabled: true,
	})
	v := s.Local()
	if got := v.LocalDB().DB.Path; got != "/tmp/test.db" {
		t.Fatalf("LocalDB().DB.Path = %q, want /tmp/test.db", got)
	}
	if !v.LocalDBEnabled() {
		t.Fatal("LocalDBEnabled() = false, want true")
	}
}

func TestStaticService_ModelsView(t *testing.T) {
	s := NewStaticService(StaticInput{
		Models: ModelsConfig{Model: "claude-3-5-sonnet"},
	})
	v := s.Models()
	if got := v.ModelsConfig().Model; got != "claude-3-5-sonnet" {
		t.Fatalf("ModelsConfig().Model = %q, want claude-3-5-sonnet", got)
	}
}

func TestStaticService_AuthView(t *testing.T) {
	s := NewStaticService(StaticInput{
		Auth: []AuthSource{
			{Name: "hugr", Type: "hugr", AccessToken: "x"},
			{Name: "oidc", Type: "oidc", Issuer: "https://issuer"},
		},
	})
	v := s.Auth()
	got := v.Sources()
	if len(got) != 2 {
		t.Fatalf("Sources() len = %d, want 2", len(got))
	}
	// Mutating returned slice MUST NOT affect the service.
	got[0].Name = "mutated"
	if again := v.Sources(); again[0].Name != "hugr" {
		t.Fatalf("Sources()[0].Name = %q after mutation, want hugr — copy-on-read broken",
			again[0].Name)
	}
}

func TestStaticService_PermissionsView(t *testing.T) {
	s := NewStaticService(StaticInput{
		Permissions: []PermissionRule{
			{Type: "hugen:tool:bash-mcp", Field: "bash.write_file", Disabled: true},
			{Type: "hugen:tool:bash-mcp", Field: "*", Data: json.RawMessage(`{"x":1}`)},
		},
	})
	v := s.Permissions()
	rules := v.Rules()
	if len(rules) != 2 {
		t.Fatalf("Rules() len = %d, want 2", len(rules))
	}
	if !rules[0].Disabled {
		t.Fatal("rules[0].Disabled = false, want true")
	}
	rules[0].Disabled = false
	if again := v.Rules(); !again[0].Disabled {
		t.Fatal("Rules() returned a shared slice — copy-on-read broken")
	}
}

func TestStaticService_ToolProvidersView(t *testing.T) {
	s := NewStaticService(StaticInput{
		ToolProviders: []ToolProviderSpec{
			{Name: "bash-mcp", Type: "stdio_mcp", Command: "/usr/local/bin/bash-mcp"},
		},
	})
	v := s.ToolProviders()
	got := v.Providers()
	if len(got) != 1 || got[0].Name != "bash-mcp" {
		t.Fatalf("Providers() = %+v, want one bash-mcp entry", got)
	}
}

func TestStaticService_OnUpdateNoop(t *testing.T) {
	s := NewStaticService(StaticInput{})
	called := 0
	cancel := s.Local().OnUpdate(func() { called++ })
	if cancel == nil {
		t.Fatal("OnUpdate returned nil cancel")
	}
	cancel() // must not panic, must not deliver any callback
	if called != 0 {
		t.Fatalf("OnUpdate fired %d times for static service, want 0", called)
	}
}

func TestStaticService_SubscribeClosesOnContextCancel(t *testing.T) {
	s := NewStaticService(StaticInput{})
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := s.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe error: %v", err)
	}
	// No event delivered for a static service — the channel only
	// closes when the context cancels.
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("Subscribe channel delivered an event for static service")
		}
	case <-time.After(time.Second):
		t.Fatal("Subscribe channel did not close after context cancel")
	}
}

// TestStaticService_SubagentsView_Defaults verifies the runtime
// defaults: MaxDepth=5, MaxAsyncMissionsPerRoot=5.
func TestStaticService_SubagentsView_Defaults(t *testing.T) {
	s := NewStaticService(StaticInput{})
	v := s.Subagents()
	if got := v.DefaultMaxDepth(); got != 5 {
		t.Errorf("DefaultMaxDepth = %d, want 5", got)
	}
	if got := v.MaxAsyncMissionsPerRoot(); got != 5 {
		t.Errorf("MaxAsyncMissionsPerRoot = %d, want 5", got)
	}
}

// TestStaticService_SubagentsView_Override verifies operator-
// supplied values flow through unmodified.
func TestStaticService_SubagentsView_Override(t *testing.T) {
	s := NewStaticService(StaticInput{
		Subagents: SubagentsConfig{
			MaxDepth:                8,
			MaxAsyncMissionsPerRoot: 12,
		},
	})
	v := s.Subagents()
	if v.DefaultMaxDepth() != 8 {
		t.Errorf("DefaultMaxDepth = %d, want 8", v.DefaultMaxDepth())
	}
	if v.MaxAsyncMissionsPerRoot() != 12 {
		t.Errorf("MaxAsyncMissionsPerRoot = %d, want 12", v.MaxAsyncMissionsPerRoot())
	}
}

// TestStaticService_TierDefaults_Materialised verifies the
// per-tier turn-loop defaults block is auto-populated with the
// runtime constants even when the operator supplies no
// `tier_defaults` block. Phase 5.2 δ.
func TestStaticService_TierDefaults_Materialised(t *testing.T) {
	s := NewStaticService(StaticInput{})
	td := s.Subagents().TierDefaults()
	for _, tier := range []string{"root", "mission", "worker"} {
		v, ok := td[tier]
		if !ok {
			t.Fatalf("tier %q missing from defaults map", tier)
		}
		if v.MaxToolTurns <= 0 || v.MaxToolTurnsHard <= 0 {
			t.Errorf("tier %q caps = %+v; want non-zero defaults", tier, v)
		}
		if v.StuckDetection.RepeatedHash <= 0 {
			t.Errorf("tier %q stuck-detection RepeatedHash = %d; want default >0",
				tier, v.StuckDetection.RepeatedHash)
		}
	}
	if td["root"].MaxToolTurns >= td["worker"].MaxToolTurns {
		t.Errorf("root soft cap %d should be smaller than worker %d",
			td["root"].MaxToolTurns, td["worker"].MaxToolTurns)
	}
}

// TestStaticService_TierDefaults_Override merges an explicit
// operator block with the runtime defaults: supplied fields win
// per-tier, absent fields inherit. Phase 5.2 δ.
func TestStaticService_TierDefaults_Override(t *testing.T) {
	off := false
	s := NewStaticService(StaticInput{
		Subagents: SubagentsConfig{
			TierDefaults: map[string]TierTurnDefaults{
				"worker": {
					MaxToolTurns:   80,
					StuckDetection: StuckPolicy{Enabled: &off},
				},
			},
		},
	})
	td := s.Subagents().TierDefaults()
	worker := td["worker"]
	if worker.MaxToolTurns != 80 {
		t.Errorf("worker.MaxToolTurns = %d; want 80 (override)", worker.MaxToolTurns)
	}
	if worker.MaxToolTurnsHard <= 0 {
		t.Errorf("worker.MaxToolTurnsHard = %d; want runtime default", worker.MaxToolTurnsHard)
	}
	if worker.StuckDetection.IsEnabled() {
		t.Error("worker stuck detection should be disabled by explicit override")
	}
	// Untouched tiers keep the runtime defaults.
	root := td["root"]
	if root.MaxToolTurns <= 0 || root.MaxToolTurnsHard <= 0 {
		t.Errorf("root defaults missing: %+v", root)
	}
}

// TestStaticService_TierDefaults_Copy ensures the accessor returns
// a defensive copy — mutating it must not bleed into subsequent
// calls.
func TestStaticService_TierDefaults_Copy(t *testing.T) {
	s := NewStaticService(StaticInput{})
	td := s.Subagents().TierDefaults()
	td["worker"] = TierTurnDefaults{MaxToolTurns: 1}
	td2 := s.Subagents().TierDefaults()
	if td2["worker"].MaxToolTurns != defaultTierWorkerMaxTurns {
		t.Errorf("worker leaked mutation; got %d, want %d",
			td2["worker"].MaxToolTurns, defaultTierWorkerMaxTurns)
	}
}

// TestStaticService_Parking_Defaults verifies ε defaults
// materialise when the operator omits the parking block.
func TestStaticService_Parking_Defaults(t *testing.T) {
	s := NewStaticService(StaticInput{})
	v := s.Subagents()
	if got := v.MaxParkedChildrenPerRoot(); got != 3 {
		t.Errorf("MaxParkedChildrenPerRoot = %d, want 3", got)
	}
	if got := v.ParkedIdleTimeout(); got != 10*time.Minute {
		t.Errorf("ParkedIdleTimeout = %v, want 10m", got)
	}
}

// TestStaticService_Parking_Override verifies operator values flow
// through unmodified.
func TestStaticService_Parking_Override(t *testing.T) {
	s := NewStaticService(StaticInput{
		Subagents: SubagentsConfig{
			Parking: ParkingConfig{
				MaxParkedChildrenPerRoot: 7,
				ParkedIdleTimeout:        90 * time.Second,
			},
		},
	})
	v := s.Subagents()
	if got := v.MaxParkedChildrenPerRoot(); got != 7 {
		t.Errorf("MaxParkedChildrenPerRoot = %d, want 7", got)
	}
	if got := v.ParkedIdleTimeout(); got != 90*time.Second {
		t.Errorf("ParkedIdleTimeout = %v, want 90s", got)
	}
}
