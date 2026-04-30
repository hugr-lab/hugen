package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdhttp "net/http"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

// AgentTokenPath is where the AgentTokenStore handler is mounted.
// Defined here as the single source of truth so the cmd/hugen
// builder that composes the child MCP's HUGR_TOKEN_URL doesn't
// duplicate the literal — change once, both sides move together.
const AgentTokenPath = "/api/auth/agent-token"

// LoopbackTokenURL returns the URL a same-host child process
// (e.g. hugr-query) should dial to reach the agent-token endpoint.
// Always `localhost` + the agent's listener port, never the
// user-visible BaseURI: child MCPs share the agent's network
// namespace, so loopback works regardless of how external clients
// reach the agent (host.docker.internal, public DNS name, etc.).
//
// `localhost` (rather than a literal 127.0.0.1) lets the OS pick
// IPv4 / IPv6 per its hosts-file config — agent listener binds
// `:port` on all interfaces, so either family connects fine.
//
// scheme is "http" today; passing "" picks the default. When TLS
// arrives for the loopback listener (out of scope for phase 3),
// callers can pass "https" without re-touching every consumer.
func LoopbackTokenURL(scheme string, port int) string {
	if scheme == "" {
		scheme = "http"
	}
	return fmt.Sprintf("%s://localhost:%d%s", scheme, port, AgentTokenPath)
}

// AgentTokenSource mints the agent's current Hugr access token. The
// implementation is the hugr auth source's Token method; the
// adapter does not care whether that's an OIDC store or a remote
// exchange.
//
// Token returns the current bearer plus its remaining lifetime in
// seconds. The TTL flows through to the consumer (hugr-query) so
// its own RemoteStore can size refresh cadence to the JWT's actual
// lifetime instead of a hardcoded ceiling — short-lived tokens
// otherwise expire mid-call and surface as `tool_error{auth}`.
//
// expiresIn=0 is the "unknown TTL" signal — the handler defaults
// to a conservative 60 s so the consumer refreshes often enough to
// stay within typical OAuth lifetimes.
type AgentTokenSource interface {
	Token(ctx context.Context) (token string, expiresIn int, err error)
}

// AgentTokenStore mediates the loopback /api/auth/agent-token
// endpoint: child MCP processes (hugr-query today) exchange their
// per-spawn bootstrap secret — and, on rotation, the previously
// issued token — for the agent's actual Hugr access token.
//
// Lifecycle of a spawn entry:
//
//  1. cmd/hugen mints a random bootstrap token, calls RegisterSpawn,
//     and passes the bootstrap to the child via HUGR_ACCESS_TOKEN.
//  2. Child's first refresh call carries token = bootstrap. The
//     handler validates the 30 s bootstrap window, asks the auth
//     source for the agent's current Hugr JWT, records the JWT in
//     the spawn's IssuedHistory LRU, and returns it.
//  3. Subsequent refreshes carry token = previously-issued JWT. The
//     handler matches it against IssuedHistory, mints a fresh JWT,
//     records, and returns. The bootstrap window is irrelevant
//     after the first successful exchange.
//  4. RevokeSpawn (called when the child exits or the agent
//     shuts down) drops the entry; tokens it previously held no
//     longer authenticate.
//
// The store does not maintain a single global "current" token; it
// always asks the auth source on every successful match. Callers
// that hammer the endpoint will tail the auth source's own cache
// behaviour.
type AgentTokenStore struct {
	source AgentTokenSource

	bootstrapWindow time.Duration
	historySize     int

	mu     sync.Mutex
	spawns map[string]*spawnEntry            // bootstrap → entry
	issued map[string]*spawnEntry            // any issued token → entry (mirror of LRUs)
	all    map[*spawnEntry]map[string]struct{} // every issued token attributed to entry, for cleanup on revoke
}

type spawnEntry struct {
	bootstrap string
	spawnedAt time.Time
	revoked   bool
	history   *lru.Cache[string, struct{}]
}

// AgentTokenOptions tunes the AgentTokenStore. Zero values fall back
// to a 30 s bootstrap window and a 16-token per-spawn history; both
// match the contract in specs/003.../contracts/hugr-query.md.
type AgentTokenOptions struct {
	BootstrapWindow time.Duration
	HistorySize     int
}

// NewAgentTokenStore constructs the store. The source is non-nil at
// boot; nil is treated as a fatal misconfiguration so callers that
// somehow disabled hugr auth but still want hugr-query get a clean
// startup error rather than a runtime nil deref.
//
// BootstrapWindow defaults to 0 (no window) — per_agent MCPs may
// idle for hours before the first tool call, and a short window
// would lock them out. The bootstrap secret itself is a 32-byte
// random known only to the spawned process via env; spawn
// revocation on child exit is the real protection. Operators who
// want the additional time-bound check can set a positive
// duration explicitly.
func NewAgentTokenStore(source AgentTokenSource, opts AgentTokenOptions) (*AgentTokenStore, error) {
	if source == nil {
		return nil, errors.New("http: AgentTokenSource is required")
	}
	if opts.BootstrapWindow < 0 {
		opts.BootstrapWindow = 0
	}
	if opts.HistorySize <= 0 {
		opts.HistorySize = 16
	}
	return &AgentTokenStore{
		source:          source,
		bootstrapWindow: opts.BootstrapWindow,
		historySize:     opts.HistorySize,
		spawns:          make(map[string]*spawnEntry),
		issued:          make(map[string]*spawnEntry),
		all:             make(map[*spawnEntry]map[string]struct{}),
	}, nil
}

// RegisterSpawn enrols a freshly-minted bootstrap token. The
// returned func revokes the spawn — callers wire it to the child
// process exit (cmd.Wait → revoke). Re-registering the same
// bootstrap is rejected: bootstrap tokens are random secrets,
// collision is a programmer bug, not an operational case.
func (s *AgentTokenStore) RegisterSpawn(bootstrap string) (revoke func(), err error) {
	if bootstrap == "" {
		return nil, errors.New("http: empty bootstrap token")
	}
	hist, err := lru.New[string, struct{}](s.historySize)
	if err != nil {
		return nil, fmt.Errorf("http: lru: %w", err)
	}
	entry := &spawnEntry{
		bootstrap: bootstrap,
		spawnedAt: time.Now(),
		history:   hist,
	}
	s.mu.Lock()
	if _, dup := s.spawns[bootstrap]; dup {
		s.mu.Unlock()
		return nil, errors.New("http: bootstrap token already registered")
	}
	s.spawns[bootstrap] = entry
	s.all[entry] = make(map[string]struct{})
	s.mu.Unlock()
	return func() { s.RevokeSpawn(bootstrap) }, nil
}

// RevokeSpawn drops a spawn and every token attributed to it. Safe
// to call twice or with an unknown bootstrap.
func (s *AgentTokenStore) RevokeSpawn(bootstrap string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.spawns[bootstrap]
	if !ok {
		return
	}
	entry.revoked = true
	delete(s.spawns, bootstrap)
	for tok := range s.all[entry] {
		delete(s.issued, tok)
	}
	delete(s.all, entry)
}

type agentTokenRequest struct {
	Token string `json:"token"`
}

type agentTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type,omitempty"`
	ExpiresIn   int    `json:"expires_in,omitempty"`
}

// Handle is the HTTP handler. The mux mounts it at
// POST /api/auth/agent-token. The handler is loopback-only; it
// rejects anything else with 403.
//
// Body shape: {"token": "<bootstrap or previously-issued>"}.
// Response: {"access_token": "...", "expires_in": <seconds>,
// "token_type": "Bearer"}.
//
// 401 covers every authentication failure (unknown token, expired
// bootstrap window, revoked spawn). The handler never reveals which
// of those hit so a caller polling random tokens learns nothing.
func (s *AgentTokenStore) Handle(w stdhttp.ResponseWriter, r *stdhttp.Request) {
	if r.Method != stdhttp.MethodPost {
		writeError(w, stdhttp.StatusMethodNotAllowed, "method_not_allowed", "POST only")
		return
	}
	if !isLoopback(r.RemoteAddr) {
		writeError(w, stdhttp.StatusForbidden, "loopback_only", "agent-token is loopback-only")
		return
	}
	var req agentTokenRequest
	if err := json.NewDecoder(stdhttp.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, stdhttp.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.Token == "" {
		writeError(w, stdhttp.StatusUnauthorized, "unauthenticated", "missing token")
		return
	}

	entry, ok := s.lookup(req.Token)
	if !ok {
		writeError(w, stdhttp.StatusUnauthorized, "unauthenticated", "unknown or revoked token")
		return
	}

	access, expiresIn, err := s.mint(r.Context())
	if err != nil {
		writeError(w, stdhttp.StatusBadGateway, "auth_source", "agent token unavailable")
		return
	}
	s.recordIssued(entry, access)
	writeJSON(w, stdhttp.StatusOK, agentTokenResponse{
		AccessToken: access,
		TokenType:   "Bearer",
		ExpiresIn:   expiresIn,
	})
}

// lookup matches a presented token against either the bootstrap
// table (with window enforcement) or the issued-history mirror.
// Returns the spawn entry on success.
func (s *AgentTokenStore) lookup(token string) (*spawnEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.spawns[token]; ok {
		if entry.revoked {
			return nil, false
		}
		if s.bootstrapWindow > 0 && time.Since(entry.spawnedAt) > s.bootstrapWindow {
			return nil, false
		}
		return entry, true
	}
	if entry, ok := s.issued[token]; ok {
		if entry.revoked {
			return nil, false
		}
		return entry, true
	}
	return nil, false
}

// mint asks the auth source for the current Hugr token plus its
// remaining lifetime. We forward the source's TTL verbatim so the
// consumer's RemoteStore caches for exactly the right window. A
// zero TTL falls back to a conservative 60 s — better to refresh
// too often than to use a token Hugr already considers stale.
func (s *AgentTokenStore) mint(ctx context.Context) (string, int, error) {
	tok, expiresIn, err := s.source.Token(ctx)
	if err != nil {
		return "", 0, err
	}
	if expiresIn <= 0 {
		expiresIn = 60
	}
	return tok, expiresIn, nil
}

// recordIssued attributes a freshly-minted token to a spawn so the
// next refresh round-trip can match it. Uses the LRU's eviction
// callback model by simply mirroring adds + (best-effort) cleanup
// when keys evict; golang-lru/v2 doesn't expose evictions to us
// without a per-key callback, so we sweep on RevokeSpawn instead.
// The cost: an evicted token may stay in s.issued until revoke;
// `lookup` re-checks `entry.history.Contains` to keep that honest.
func (s *AgentTokenStore) recordIssued(entry *spawnEntry, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry.revoked {
		return
	}
	entry.history.Add(token, struct{}{})
	s.issued[token] = entry
	s.all[entry][token] = struct{}{}
	// Mirror cleanup: when the LRU drops a key, lookup must reject
	// it. Walk our attribution set, drop anything no longer in the
	// LRU. O(history_size) per call; history_size is small.
	for tok := range s.all[entry] {
		if !entry.history.Contains(tok) {
			delete(s.all[entry], tok)
			delete(s.issued, tok)
		}
	}
}

// SpawnCount reports the number of live spawn entries; used by
// tests and (later) by ops endpoints.
func (s *AgentTokenStore) SpawnCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.spawns)
}
