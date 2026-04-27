package auth

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
)

// Source is a stateful token provider. It may require an
// interactive login flow (OAuth via browser) or just refresh a
// pre-seeded token (RemoteStore). Each Source declares its own
// callback handling; a single SourceRegistry dispatches
// /auth/callback requests to the owning Source by state prefix.
//
// Every Source also implements TokenStore — callers that only
// need a token can keep using auth.Transport(src, base).
type Source interface {
	// Name is the registry key (matches AuthSpec.Name).
	Name() string

	// Token returns a valid access token. Blocks until the first
	// login completes for OAuth Sources.
	Token(ctx context.Context) (string, error)

	// Login kicks off the browser flow (writes login URL to log,
	// optionally opens browser). No-op for token-mode Sources.
	Login(ctx context.Context) error

	// OwnsState reports whether the given OAuth state parameter
	// belongs to this Source. Default convention is a prefix match
	// of "<Name>." — StateOwnedBy helps implementers follow it.
	OwnsState(state string) bool

	// HandleCallback completes the OAuth flow for a callback
	// request that OwnsState returned true for. Sources that don't
	// participate in browser flows return a 400 from this method.
	HandleCallback(w http.ResponseWriter, r *http.Request)
}

// EncodeState returns a state parameter scoped to a Source by
// prefixing the random nonce with the Source name. The dispatcher
// reads the prefix to route the callback.
func EncodeState(name, nonce string) string {
	return name + "." + nonce
}

// StateOwnedBy reports whether a state belongs to the named Source
// under the default "<name>." encoding. Sources may call it from
// their OwnsState implementations.
func StateOwnedBy(name, state string) bool {
	return strings.HasPrefix(state, name+".")
}

// SourceRegistry owns the set of active Sources and mounts a single
// /auth/callback dispatcher that routes by OAuth state prefix.
//
// Mount registers routes on the provided mux. Add/Alias can be
// called either before or after Mount — the dispatcher looks up the
// owning Source dynamically on each request.
type SourceRegistry struct {
	logger *slog.Logger

	mu           sync.RWMutex
	byName       map[string]Source
	aliases      map[string]string // alias -> target source name
	primary      string            // name of the primary Source (set via AddPrimary)
	promptLogins []func()
}

// NewSourceRegistry creates an empty registry.
func NewSourceRegistry(logger *slog.Logger) *SourceRegistry {
	if logger == nil {
		logger = slog.Default()
	}
	return &SourceRegistry{
		logger:  logger,
		byName:  make(map[string]Source),
		aliases: make(map[string]string),
	}
}

// Add registers a Source under its Name. Returns an error on
// duplicate name or alias collision.
func (r *SourceRegistry) Add(s Source) error {
	return r.add(s, false)
}

// AddPrimary is Add + marks the Source as the primary (hugr-side)
// Source. BuildSources uses the primary when resolving alias
// entries (`type: hugr`) without depending on a hardcoded name.
// Only one Source can be primary — a second call returns an error.
func (r *SourceRegistry) AddPrimary(s Source) error {
	return r.add(s, true)
}

func (r *SourceRegistry) add(s Source, primary bool) error {
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
func (r *SourceRegistry) Primary() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.primary
}

// Alias registers `name` as a pointer to an existing Source. Used
// by provider-auth entries with type=hugr that reuse the main hugr
// connection's token without a separate Source.
func (r *SourceRegistry) Alias(name, target string) error {
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
func (r *SourceRegistry) Source(name string) (Source, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sourceLocked(name)
}

// TokenStore returns the named Source as a TokenStore. Convenience
// for auth.Transport callers that only need Token().
func (r *SourceRegistry) TokenStore(name string) (TokenStore, bool) {
	s, ok := r.Source(name)
	if !ok {
		return nil, false
	}
	return s, true
}

// TokenStores returns a snapshot of every registered name → Source
// mapping, including aliases. Kept for callers (tools.MCPSpec) that
// still expect a map[string]TokenStore.
func (r *SourceRegistry) TokenStores() map[string]TokenStore {
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

// Mount installs /auth/callback on the given mux. Login paths are
// mounted per-Source when a Source exposes one via
// LoginPathProvider. Safe to call once per mux.
func (r *SourceRegistry) Mount(mux *http.ServeMux) {
	if mux == nil {
		return
	}
	mux.HandleFunc("/auth/callback", r.dispatchCallback)

	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.byName {
		if lp, ok := s.(LoginPathProvider); ok {
			path := lp.LoginPath()
			if path == "" {
				continue
			}
			src := s
			mux.HandleFunc(path, func(w http.ResponseWriter, req *http.Request) {
				if lh, ok := src.(LoginHandler); ok {
					lh.HandleLogin(w, req)
					return
				}
				http.Error(w, "login not supported", http.StatusNotFound)
			})
		}
	}
}

// RegisterPromptLogin queues a startup hook that prints a login URL
// to the console once the HTTP listener is bound. Typically invoked
// from Source constructors.
func (r *SourceRegistry) RegisterPromptLogin(f func()) {
	if f == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.promptLogins = append(r.promptLogins, f)
}

// PromptLogins returns the prompt-login hooks that callers should
// fire once the HTTP listener is bound.
func (r *SourceRegistry) PromptLogins() []func() {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]func(), len(r.promptLogins))
	copy(out, r.promptLogins)
	return out
}

// dispatchCallback routes /auth/callback to the Source that owns
// the OAuth state parameter.
func (r *SourceRegistry) dispatchCallback(w http.ResponseWriter, req *http.Request) {
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

func (r *SourceRegistry) ownerLocked(state string) Source {
	for _, s := range r.byName {
		if s.OwnsState(state) {
			return s
		}
	}
	return nil
}

func (r *SourceRegistry) sourceLocked(name string) (Source, bool) {
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

// LoginPathProvider is implemented by Sources that expose a browser
// login path. Registry mounts that path on the shared mux.
type LoginPathProvider interface {
	LoginPath() string
}

// LoginHandler is implemented by Sources that handle the browser
// login request. Registry delegates to it when the LoginPath
// route fires.
type LoginHandler interface {
	HandleLogin(w http.ResponseWriter, r *http.Request)
}

func prefixOf(state string) string {
	if i := strings.IndexByte(state, '.'); i > 0 {
		return state[:i]
	}
	return ""
}
