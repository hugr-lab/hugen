package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/hugr-lab/hugen/pkg/auth/sources"
)

// AgentTokenPath is the loopback endpoint stdio MCP children call
// to refresh the agent's Hugr access token.
const AgentTokenPath = "/api/auth/agent-token"

// StdioAuth carries the credentials a stdio MCP child needs to keep
// its Hugr bearer fresh through the loopback /api/auth/agent-token
// endpoint.
//
// Lifecycle:
//
//   - The child reads BootstrapToken (env HUGR_ACCESS_TOKEN) and
//     TokenURL (env HUGR_TOKEN_URL) at start, performs a first
//     exchange to obtain the agent's current Hugr JWT, and rotates
//     thereafter using each previously issued JWT as the proof.
//   - The caller MUST run RevokeFunc when the child exits — without
//     it, JWTs the spawn was issued continue to authenticate.
type StdioAuth struct {
	BootstrapToken string
	TokenURL       string
	RevokeFunc     func()
}

// Env renders the credentials into the env-key contract every
// Hugr-aware MCP child reads. Callers merge this into the spawn env
// without knowing the keys.
func (s StdioAuth) Env() map[string]string {
	return map[string]string{
		"HUGR_TOKEN_URL":    s.TokenURL,
		"HUGR_ACCESS_TOKEN": s.BootstrapToken,
	}
}

// NewStdioAuth registers a fresh bootstrap for a stdio child of the
// named auth source. Only the primary source is supported today;
// non-primary names — and deployments without a hugr-flavoured
// primary — return a clear error so misconfiguration surfaces at
// boot, not silently mid-call.
func (s *Service) NewStdioAuth(_ context.Context, name string) (StdioAuth, error) {
	if name == "" {
		return StdioAuth{}, errors.New("auth: NewStdioAuth requires a non-empty source name")
	}
	s.mu.RLock()
	store := s.agentTokens
	primary := s.primary
	s.mu.RUnlock()
	if name != primary {
		return StdioAuth{}, fmt.Errorf("auth source %q: stdio spawn injection only supported for the primary source", name)
	}
	if store == nil {
		return StdioAuth{}, fmt.Errorf("auth source %q: agent-token store not configured (no-Hugr deployment?)", name)
	}
	bootstrap, err := mintBootstrap()
	if err != nil {
		return StdioAuth{}, fmt.Errorf("auth source %q: mint bootstrap: %w", name, err)
	}
	revoke, err := store.registerSpawn(bootstrap)
	if err != nil {
		return StdioAuth{}, fmt.Errorf("auth source %q: register spawn: %w", name, err)
	}
	return StdioAuth{
		BootstrapToken: bootstrap,
		TokenURL:       loopbackTokenURL(s.loopbackPort),
		RevokeFunc:     revoke,
	}, nil
}

// attachAgentTokensLocked is called from Service.add under r.mu when
// a primary source is registered. It builds the loopback store and
// mounts the handler iff the source exposes TokenWithTTL — only
// hugr-flavoured sources do today, so non-hugr primaries leave the
// path unmounted and NewStdioAuth fails with a clear error.
func (s *Service) attachAgentTokensLocked(src sources.Source) {
	tt, ok := src.(ttlAware)
	if !ok {
		return
	}
	store := newAgentTokenStore(tt)
	s.mux.Handle(AgentTokenPath, http.HandlerFunc(store.handle))
	s.agentTokens = store
}

// ttlAware is implemented by Sources that report the remaining
// lifetime of their cached token. Forwarding the TTL through the
// loopback lets the consumer (hugr-query's RemoteStore) refresh on
// the JWT's actual cadence rather than a hardcoded ceiling.
type ttlAware interface {
	TokenWithTTL(ctx context.Context) (string, int, error)
}

// loopbackTokenURL returns the URL a same-host child should dial.
// localhost (rather than 127.0.0.1) lets the OS pick IPv4/IPv6 per
// its hosts-file config — the listener binds `:port` on all
// interfaces, so either family connects fine.
func loopbackTokenURL(port int) string {
	return fmt.Sprintf("http://localhost:%d%s", port, AgentTokenPath)
}

// agentTokenStore mediates the loopback /api/auth/agent-token
// endpoint: child MCP processes exchange their per-spawn bootstrap
// secret — and, on rotation, the previously issued token — for the
// agent's actual Hugr access token.
//
// Lifecycle of a spawn entry:
//
//  1. Service.NewStdioAuth mints a random bootstrap, registers it,
//     and hands it to the caller; the caller passes it to the child
//     via env.
//  2. Child's first refresh call carries token = bootstrap. The
//     handler asks the source for the agent's current JWT, records
//     the JWT in the spawn's IssuedHistory LRU, and returns it.
//  3. Subsequent refreshes carry token = previously-issued JWT. The
//     handler matches, mints fresh, records, returns.
//  4. The revoke callback returned to the caller drops the entry;
//     tokens it previously held no longer authenticate.
type agentTokenStore struct {
	source ttlAware

	historySize int

	mu     sync.Mutex
	spawns map[string]*spawnEntry              // bootstrap → entry
	issued map[string]*spawnEntry              // any issued token → entry
	all    map[*spawnEntry]map[string]struct{} // every issued token attributed to entry, for cleanup on revoke
}

type spawnEntry struct {
	bootstrap string
	revoked   bool
	history   *lru.Cache[string, struct{}]
}

func newAgentTokenStore(src ttlAware) *agentTokenStore {
	return &agentTokenStore{
		source:      src,
		historySize: 16,
		spawns:      make(map[string]*spawnEntry),
		issued:      make(map[string]*spawnEntry),
		all:         make(map[*spawnEntry]map[string]struct{}),
	}
}

func (s *agentTokenStore) registerSpawn(bootstrap string) (func(), error) {
	if bootstrap == "" {
		return nil, errors.New("auth: empty bootstrap token")
	}
	hist, err := lru.New[string, struct{}](s.historySize)
	if err != nil {
		return nil, fmt.Errorf("auth: lru: %w", err)
	}
	entry := &spawnEntry{bootstrap: bootstrap, history: hist}
	s.mu.Lock()
	if _, dup := s.spawns[bootstrap]; dup {
		s.mu.Unlock()
		return nil, errors.New("auth: bootstrap token already registered")
	}
	s.spawns[bootstrap] = entry
	s.all[entry] = make(map[string]struct{})
	s.mu.Unlock()
	return func() { s.revokeSpawn(bootstrap) }, nil
}

func (s *agentTokenStore) revokeSpawn(bootstrap string) {
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

func (s *agentTokenStore) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAgentTokenError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
		return
	}
	if !isLoopbackAddr(r.RemoteAddr) {
		writeAgentTokenError(w, http.StatusForbidden, "loopback_only", "agent-token is loopback-only")
		return
	}
	var req agentTokenRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeAgentTokenError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.Token == "" {
		writeAgentTokenError(w, http.StatusUnauthorized, "unauthenticated", "missing token")
		return
	}
	entry, ok := s.lookup(req.Token)
	if !ok {
		writeAgentTokenError(w, http.StatusUnauthorized, "unauthenticated", "unknown or revoked token")
		return
	}
	access, expiresIn, err := s.source.TokenWithTTL(r.Context())
	if err != nil {
		writeAgentTokenError(w, http.StatusBadGateway, "auth_source", "agent token unavailable")
		return
	}
	if expiresIn <= 0 {
		expiresIn = 60
	}
	s.recordIssued(entry, access)
	writeAgentTokenJSON(w, http.StatusOK, agentTokenResponse{
		AccessToken: access,
		TokenType:   "Bearer",
		ExpiresIn:   expiresIn,
	})
}

func (s *agentTokenStore) lookup(token string) (*spawnEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.spawns[token]; ok {
		if entry.revoked {
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

// recordIssued attributes a freshly-minted token to a spawn so the
// next refresh round-trip can match it. golang-lru/v2 doesn't expose
// evictions to us without a per-key callback, so we sweep on every
// add: walk the attribution set, drop anything no longer in the LRU.
// O(history_size) per call; history_size is small.
func (s *agentTokenStore) recordIssued(entry *spawnEntry, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry.revoked {
		return
	}
	entry.history.Add(token, struct{}{})
	s.issued[token] = entry
	s.all[entry][token] = struct{}{}
	for tok := range s.all[entry] {
		if !entry.history.Contains(tok) {
			delete(s.all[entry], tok)
			delete(s.issued, tok)
		}
	}
}

// mintBootstrap returns 32 bytes of hex-encoded random — long enough
// that an attacker can't guess it before the spawn is revoked.
func mintBootstrap() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func isLoopbackAddr(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeAgentTokenJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

type agentTokenErrorEnvelope struct {
	Error agentTokenErrorBody `json:"error"`
}

type agentTokenErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeAgentTokenError(w http.ResponseWriter, status int, code, msg string) {
	writeAgentTokenJSON(w, status, agentTokenErrorEnvelope{Error: agentTokenErrorBody{Code: code, Message: msg}})
}
