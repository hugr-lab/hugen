package runtime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func validConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		Logger:   discardLogger(),
		Mode:     "local",
		StateDir: t.TempDir(),
	}
}

func TestConfig_Validate_OK(t *testing.T) {
	if err := validConfig(t).Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestConfig_Validate_Errors(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*Config)
		want string
	}{
		{"nil-logger", func(c *Config) { c.Logger = nil }, "logger is nil"},
		{"empty-state-dir", func(c *Config) { c.StateDir = "" }, "state dir is empty"},
		{"bad-mode", func(c *Config) { c.Mode = "weird" }, "mode must be local or remote"},
		{"remote-no-url", func(c *Config) { c.Mode = "remote" }, "hugr url is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig(t)
			tc.mod(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("not ErrInvalidConfig: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("missing detail %q in %v", tc.want, err)
			}
		})
	}
}

func TestBuild_RejectsInvalidConfig(t *testing.T) {
	_, err := Build(context.Background(), Config{})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig, got %v", err)
	}
}

func TestBuild_WrapsPhaseError(t *testing.T) {
	// Local mode + valid skeleton fields but no AgentConfigPath →
	// phase 4 (storage) fails when BuildConfigService cannot find
	// models.model. The wrapper should prefix "runtime: storage:".
	cfg := validConfig(t)
	_, err := Build(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected phase error, got nil")
	}
	if !strings.Contains(err.Error(), "runtime: storage:") {
		t.Errorf("missing phase wrap in %v", err)
	}
}

func TestCore_Shutdown_Idempotent(t *testing.T) {
	core := &Core{Logger: discardLogger()}
	calls := 0
	core.addCleanup(func() { calls++ })

	core.Shutdown(context.Background())
	core.Shutdown(context.Background())

	if calls != 1 {
		t.Errorf("cleanup ran %d times, want 1", calls)
	}
}

func TestCore_CleanupPartial_ReverseOrder(t *testing.T) {
	core := &Core{Logger: discardLogger()}
	var order []int
	core.addCleanup(func() { order = append(order, 1) })
	core.addCleanup(func() { order = append(order, 2) })
	core.addCleanup(func() { order = append(order, 3) })

	core.cleanupPartial()

	if len(order) != 3 || order[0] != 3 || order[1] != 2 || order[2] != 1 {
		t.Errorf("cleanup order = %v, want [3 2 1]", order)
	}
}
