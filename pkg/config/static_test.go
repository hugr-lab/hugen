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
// defaults from phase-4-spec §3 step 10: max_depth=5, max_turns=15,
// repeated_hash=3, tight_density_count=3, tight_density_window=2s,
// stuck-detection enabled.
func TestStaticService_SubagentsView_Defaults(t *testing.T) {
	s := NewStaticService(StaticInput{})
	v := s.Subagents()
	if got := v.DefaultMaxDepth(); got != 5 {
		t.Errorf("DefaultMaxDepth = %d, want 5", got)
	}
	if got := v.DefaultMaxTurns(); got != 15 {
		t.Errorf("DefaultMaxTurns = %d, want 15", got)
	}
	sd := v.DefaultStuckDetection()
	if sd.RepeatedHash != 3 {
		t.Errorf("RepeatedHash = %d, want 3", sd.RepeatedHash)
	}
	if sd.TightDensityCount != 3 {
		t.Errorf("TightDensityCount = %d, want 3", sd.TightDensityCount)
	}
	if sd.TightDensityWindow != 2*time.Second {
		t.Errorf("TightDensityWindow = %v, want 2s", sd.TightDensityWindow)
	}
	if !sd.IsEnabled() {
		t.Error("IsEnabled() = false on default; want true")
	}
}

// TestStaticService_SubagentsView_Override verifies operator-supplied
// values flow through unmodified, and an explicit Enabled=&false
// disables the heuristics.
func TestStaticService_SubagentsView_Override(t *testing.T) {
	off := false
	s := NewStaticService(StaticInput{
		Subagents: SubagentsConfig{
			MaxDepth: 8,
			MaxTurns: 25,
			StuckDetection: StuckPolicy{
				RepeatedHash:       4,
				TightDensityCount:  6,
				TightDensityWindow: 5 * time.Second,
				Enabled:            &off,
			},
		},
	})
	v := s.Subagents()
	if v.DefaultMaxDepth() != 8 || v.DefaultMaxTurns() != 25 {
		t.Errorf("MaxDepth/MaxTurns = (%d,%d), want (8,25)",
			v.DefaultMaxDepth(), v.DefaultMaxTurns())
	}
	sd := v.DefaultStuckDetection()
	if sd.RepeatedHash != 4 || sd.TightDensityCount != 6 || sd.TightDensityWindow != 5*time.Second {
		t.Errorf("StuckDetection = %+v, want overrides applied", sd)
	}
	if sd.IsEnabled() {
		t.Error("IsEnabled() = true with explicit Enabled=&false; want false")
	}
}
