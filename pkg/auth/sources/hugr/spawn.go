package hugr

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	httpapi "github.com/hugr-lab/hugen/pkg/adapter/http"
)

// Spawner is the spawn.Source implementation for `auth: hugr`.
//
// On Env it mints a random 32-byte bootstrap token, registers it
// with the AgentTokenStore, and returns the env layout
// hugr-aware MCP children expect:
//
//	HUGR_TOKEN_URL    — http://localhost:<port>/api/auth/agent-token
//	HUGR_ACCESS_TOKEN — the bootstrap; the child swaps it for a
//	                    real Hugr JWT on first refresh, then keeps
//	                    rolling it forward via that endpoint
//
// The revoke fn drops the spawn from the store; lifecycle calls
// it on session close.
//
// Spawner is constructed at boot when the deployment carries a
// `hugr` auth source — without that store, no per-spawn token
// machinery is available and the registry stays without it (so
// any provider declaring `auth: hugr` fails fast at the Lifecycle
// validation step).
type Spawner struct {
	store        *httpapi.AgentTokenStore
	loopbackPort int
}

// NewSpawner wires a Spawner around an existing AgentTokenStore.
// Both store and loopbackPort are required: the store mediates
// bootstrap → JWT exchange, the port lets the child reach the
// loopback endpoint regardless of how external clients reach the
// agent.
func NewSpawner(store *httpapi.AgentTokenStore, loopbackPort int) *Spawner {
	return &Spawner{store: store, loopbackPort: loopbackPort}
}

// Name is the YAML key consumers use: `auth: hugr`.
func (*Spawner) Name() string { return "hugr" }

// Env mints credentials for one MCP spawn. sessionID is unused by
// the hugr source today — the bootstrap token is per-spawn, not
// per-session — but the parameter is part of the spawn.Source
// contract so future sources (per-user OIDC, per-session OAuth
// proxies) can scope by it without a signature break.
func (s *Spawner) Env(_ context.Context, _ string) (map[string]string, func(), error) {
	if s.store == nil {
		return nil, nil, fmt.Errorf("auth source %q: token store not configured", s.Name())
	}
	bootstrap, err := mintBootstrap()
	if err != nil {
		return nil, nil, fmt.Errorf("auth source %q: mint bootstrap: %w", s.Name(), err)
	}
	revoke, err := s.store.RegisterSpawn(bootstrap)
	if err != nil {
		return nil, nil, fmt.Errorf("auth source %q: register spawn: %w", s.Name(), err)
	}
	env := map[string]string{
		"HUGR_TOKEN_URL":    httpapi.LoopbackTokenURL("", s.loopbackPort),
		"HUGR_ACCESS_TOKEN": bootstrap,
	}
	return env, revoke, nil
}

// mintBootstrap returns 32 bytes of hex-encoded random — long
// enough that an attacker can't guess it inside the
// AgentTokenStore's bootstrap window.
func mintBootstrap() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
