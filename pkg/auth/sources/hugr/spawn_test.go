package hugr

import (
	"context"
	"strings"
	"testing"

	httpapi "github.com/hugr-lab/hugen/pkg/adapter/http"
)

type fakeTokenSource struct{}

func (fakeTokenSource) Token(_ context.Context) (string, int, error) {
	return "agent-jwt", 60, nil
}

func TestSpawner_Env_Success(t *testing.T) {
	store, err := httpapi.NewAgentTokenStore(fakeTokenSource{}, httpapi.AgentTokenOptions{})
	if err != nil {
		t.Fatalf("NewAgentTokenStore: %v", err)
	}
	sp := NewSpawner(store, 8081)

	env, revoke, err := sp.Env(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("Env: %v", err)
	}
	if revoke == nil {
		t.Fatalf("revoke is nil")
	}
	t.Cleanup(revoke)

	if env["HUGR_ACCESS_TOKEN"] == "" {
		t.Errorf("missing HUGR_ACCESS_TOKEN")
	}
	if env["HUGR_TOKEN_URL"] == "" {
		t.Errorf("missing HUGR_TOKEN_URL")
	}
	if !strings.Contains(env["HUGR_TOKEN_URL"], ":8081/") {
		t.Errorf("HUGR_TOKEN_URL = %q does not embed port", env["HUGR_TOKEN_URL"])
	}
	if store.SpawnCount() != 1 {
		t.Errorf("SpawnCount = %d, want 1", store.SpawnCount())
	}

	// Two spawns must yield distinct bootstrap tokens.
	env2, revoke2, err := sp.Env(context.Background(), "sess-2")
	if err != nil {
		t.Fatalf("second Env: %v", err)
	}
	t.Cleanup(revoke2)
	if env["HUGR_ACCESS_TOKEN"] == env2["HUGR_ACCESS_TOKEN"] {
		t.Errorf("two spawns share a bootstrap token")
	}
}

func TestSpawner_Env_NoStore(t *testing.T) {
	sp := NewSpawner(nil, 8081)
	_, _, err := sp.Env(context.Background(), "sess-x")
	if err == nil || !strings.Contains(err.Error(), "token store not configured") {
		t.Fatalf("err = %v, want token store not configured", err)
	}
}

func TestSpawner_Name(t *testing.T) {
	if (&Spawner{}).Name() != "hugr" {
		t.Fatalf("Name should be 'hugr'")
	}
}
