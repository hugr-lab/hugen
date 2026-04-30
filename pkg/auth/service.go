package auth

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/hugr-lab/hugen/pkg/auth/sources"
)

// Service owns the set of active Sources and mounts a single
// /auth/callback dispatcher that routes by OAuth state prefix.
//
// Mount registers routes on the provided mux. Add/Alias can be
// called either before or after Mount — the dispatcher looks up the
// owning Source dynamically on each request.
type Service struct {
	logger  *slog.Logger
	baseURL string

	mu           sync.RWMutex
	byName       map[string]sources.Source
	aliases      map[string]string // alias -> target source name
	primary      string            // name of the primary Source (set via AddPrimary)
	promptLogins []func()

	mux *http.ServeMux
}

// NewService creates an empty Service.
//
// baseURL is the public origin of the hugen process — used when
// LoadFromView builds OIDC sources to derive their redirect URL
// ("<baseURL>/auth/callback").
func NewService(logger *slog.Logger, mux *http.ServeMux, baseURL string) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Service{
		logger:  logger,
		baseURL: baseURL,
		byName:  make(map[string]sources.Source),
		aliases: make(map[string]string),
		mux:     mux,
	}
	s.mount()
	return s
}

// Add registers a Source under its Name. Returns an error on
// duplicate name or alias collision.
func (r *Service) Add(s sources.Source) error {
	return r.add(s, false)
}

// AddPrimary is Add + marks the Source as the primary (hugr-side)
// Source. LoadFromView uses the primary when resolving alias
// entries (`type: hugr`) without depending on a hardcoded name.
// Only one Source can be primary — a second call returns an error.
func (r *Service) AddPrimary(s sources.Source) error {
	return r.add(s, true)
}

func (r *Service) add(s sources.Source, primary bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	name := s.Name()
	if name == "" {
		return fmt.Errorf("auth: Source has empty name")
	}
	if _, dup := r.byName[name]; dup {
		return fmt.Errorf("auth: duplicate source name %q", name)
	}
	if _, dup := r.aliases[name]; dup {
		return fmt.Errorf("auth: name %q collides with an existing alias", name)
	}
	if primary {
		if r.primary != "" {
			return fmt.Errorf("auth: primary Source already registered (%q)", r.primary)
		}
		r.primary = name
	}
	r.byName[name] = s
	return nil
}

// Primary returns the name of the primary Source, or "" when none
// has been registered via AddPrimary.
func (r *Service) Primary() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.primary
}

// Alias registers `name` as a pointer to an existing Source. Used
// by provider-auth entries with type=hugr that reuse the main hugr
// connection's token without a separate Source.
func (r *Service) Alias(name, target string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if name == "" {
		return fmt.Errorf("auth: alias name is empty")
	}
	if name == target {
		return fmt.Errorf("auth: alias %q points to itself", name)
	}
	if _, exists := r.byName[target]; !exists {
		return fmt.Errorf("auth: alias %q target %q does not exist", name, target)
	}
	if _, dup := r.byName[name]; dup {
		return fmt.Errorf("auth: alias %q collides with an existing source", name)
	}
	if _, dup := r.aliases[name]; dup {
		return fmt.Errorf("auth: duplicate alias %q", name)
	}
	r.aliases[name] = target
	return nil
}

// Source returns the registered Source by name, resolving aliases.
func (r *Service) Source(name string) (sources.Source, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sourceLocked(name)
}

// TokenStore returns the named Source as a TokenStore. Convenience
// for auth.Transport callers that only need Token().
func (r *Service) TokenStore(name string) (TokenStore, bool) {
	s, ok := r.Source(name)
	if !ok {
		return nil, false
	}
	return s, true
}

// TokenStores returns a snapshot of every registered name → Source
// mapping, including aliases. Kept for callers (tools.MCPSpec) that
// still expect a map[string]TokenStore.
func (r *Service) TokenStores() map[string]TokenStore {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]TokenStore, len(r.byName)+len(r.aliases))
	for n, s := range r.byName {
		out[n] = s
	}
	for a, tgt := range r.aliases {
		if s, ok := r.byName[tgt]; ok {
			out[a] = s
		}
	}
	return out
}

// Registers auth endpoints /auth/callback and /auth/login/{name}.
func (r *Service) mount() {
	r.mux.HandleFunc("/auth/callback", r.dispatchCallback)
	r.mux.HandleFunc("/auth/login/{name}", r.dispatchLogin)
}

// RegisterPromptLogin queues a startup hook that prints a login URL
// to the console once the HTTP listener is bound. Typically invoked
// from Source constructors.
func (r *Service) RegisterPromptLogin(f func()) {
	if f == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.promptLogins = append(r.promptLogins, f)
}

// PromptLogins returns the prompt-login hooks that callers should
// fire once the HTTP listener is bound.
func (r *Service) PromptLogins() []func() {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]func(), len(r.promptLogins))
	copy(out, r.promptLogins)
	return out
}

// dispatchCallback routes /auth/callback to the Source that owns
// the OAuth state parameter.
func (r *Service) dispatchCallback(w http.ResponseWriter, req *http.Request) {
	state := req.URL.Query().Get("state")
	if state == "" {
		http.Error(w, "missing state", http.StatusBadRequest)
		return
	}

	r.mu.RLock()
	owner := r.ownerLocked(state)
	r.mu.RUnlock()

	if owner == nil {
		r.logger.Warn("auth: callback with unknown state", "state_prefix", prefixOf(state))
		http.Error(w, "unknown state", http.StatusBadRequest)
		return
	}
	owner.HandleCallback(w, req)
}

func (r *Service) dispatchLogin(w http.ResponseWriter, req *http.Request) {
	name := req.PathValue("name")
	r.mu.RLock()
	source, ok := r.sourceLocked(name)
	r.mu.RUnlock()
	if !ok {
		http.NotFound(w, req)
		return
	}

	if lh, ok := source.(sources.LoginHandler); ok {
		lh.HandleLogin(w, req)
		return
	}
	http.Error(w, "login not supported", http.StatusNotFound)
}

func (r *Service) ownerLocked(state string) sources.Source {
	for _, s := range r.byName {
		if s.OwnsState(state) {
			return s
		}
	}
	return nil
}

func (r *Service) sourceLocked(name string) (sources.Source, bool) {
	if s, ok := r.byName[name]; ok {
		return s, true
	}
	if tgt, ok := r.aliases[name]; ok {
		if s, ok := r.byName[tgt]; ok {
			return s, true
		}
	}
	return nil, false
}

func prefixOf(state string) string {
	if i := strings.IndexByte(state, '.'); i > 0 {
		return state[:i]
	}
	return ""
}
