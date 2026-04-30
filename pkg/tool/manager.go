package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/skill"
)

// ToolManager dispatches tool calls through the three permission
// tiers and the registered providers. One instance per agent;
// per-session catalogue is materialised via Snapshot.
//
// Two scopes of providers are supported:
//   - global: registered via AddProvider, visible to every session.
//     System tools and remote-shared MCPs (e.g. hugr-main) live here.
//   - session: registered via AddSessionProvider for a specific
//     sessionID. Per-session bash-mcp / python-mcp / duckdb-mcp
//     live here so each session has its own subprocess scoped to
//     its own workspace directory. Session-scoped providers
//     shadow global providers on Dispatch (a session can override
//     a global "bash-mcp" with its own); for Snapshot they are
//     merged by name (session wins on collision).
type ToolManager struct {
	perms  perm.Service
	skills *skill.SkillManager
	log    *slog.Logger

	providersView  config.ToolProvidersView
	authResolver   AuthResolver
	connectTimeout time.Duration

	mu               sync.RWMutex
	providers        map[string]ToolProvider
	sessionProviders map[string]map[string]ToolProvider // sessionID → name → provider
	cache            map[string]*cachedSnapshot

	toolGen   atomic.Int64
	policyGen atomic.Int64

	drainTimeout time.Duration
}

type cachedSnapshot struct {
	gens Generations
	snap Snapshot
}

// Snapshot is the per-Turn frozen catalogue for a session.
type Snapshot struct {
	Generations Generations
	Tools       []Tool
}

// Generations is the (skill_gen, tool_gen, policy_gen) triple
// keying each Snapshot. ToolManager rebuilds when any field
// moves.
type Generations struct {
	Skill  int64
	Tool   int64
	Policy int64
}

const (
	defaultDrainTimeout   = 5 * time.Second
	defaultConnectTimeout = 30 * time.Second
)

// NewToolManager constructs the manager. Construction is cheap —
// no providers are connected here; call Init(ctx) when ready to
// open the configured MCP connections.
//
// Args:
//   - perms: permission service consulted on every Dispatch.
//   - skills: skill manager consulted to filter the per-Turn tool
//     catalogue (nil disables filtering — used by tests).
//   - providers: per_agent MCP catalogue view. Read on Init()
//     (and, in phase 6+, again on view.OnUpdate). Per_session
//     entries are skipped (registered via AddSessionProvider).
//     Pass nil if no MCP entries are configured.
//   - resolver: maps named auth sources to bearer-injecting
//     RoundTrippers. Required when any HTTP provider declares
//     `auth: <name>`; pass nil if no provider needs auth.
//   - log: structured logger; nil falls back to a discard handler.
func NewToolManager(
	perms perm.Service,
	skills *skill.SkillManager,
	providers config.ToolProvidersView,
	resolver AuthResolver,
	log *slog.Logger,
) *ToolManager {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &ToolManager{
		perms:            perms,
		skills:           skills,
		log:              log,
		providersView:    providers,
		authResolver:     resolver,
		connectTimeout:   defaultConnectTimeout,
		providers:        make(map[string]ToolProvider),
		sessionProviders: make(map[string]map[string]ToolProvider),
		cache:            make(map[string]*cachedSnapshot),
		drainTimeout:     defaultDrainTimeout,
	}
}

// AddSessionProvider registers a provider scoped to one session.
// Used for MCPs whose lifecycle is tied to the session (e.g.
// per-session bash-mcp running in the session's workspace dir).
// Returns an error if a session-scoped provider with the same
// name is already registered for that session.
func (m *ToolManager) AddSessionProvider(sessionID string, p ToolProvider) error {
	if sessionID == "" {
		return errors.New("tool: empty session id")
	}
	if p == nil {
		return errors.New("tool: nil provider")
	}
	name := p.Name()
	if name == "" {
		return errors.New("tool: provider with empty name")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	scoped, ok := m.sessionProviders[sessionID]
	if !ok {
		scoped = make(map[string]ToolProvider)
		m.sessionProviders[sessionID] = scoped
	}
	if _, exists := scoped[name]; exists {
		return fmt.Errorf("tool: provider %q already registered for session %s", name, sessionID)
	}
	scoped[name] = p
	delete(m.cache, sessionID) // force rebuild for this session
	return nil
}

// RemoveSessionProvider drains in-flight calls before disposing
// the named provider for one session. Idempotent: returns nil if
// nothing is registered. Other sessions and global providers are
// untouched.
func (m *ToolManager) RemoveSessionProvider(ctx context.Context, sessionID, name string) error {
	m.mu.Lock()
	scoped, ok := m.sessionProviders[sessionID]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	p, ok := scoped[name]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	delete(scoped, name)
	if len(scoped) == 0 {
		delete(m.sessionProviders, sessionID)
	}
	delete(m.cache, sessionID)
	m.mu.Unlock()

	dctx, cancel := context.WithTimeout(ctx, m.drainTimeout)
	defer cancel()
	<-dctx.Done()
	if err := p.Close(); err != nil {
		return fmt.Errorf("tool: close session %s/%s: %w", sessionID, name, err)
	}
	return nil
}

// CloseSession tears down every provider registered for sessionID.
// Returns the joined errors from each Close call. Used by the
// session lifecycle on Close.
func (m *ToolManager) CloseSession(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	scoped := m.sessionProviders[sessionID]
	delete(m.sessionProviders, sessionID)
	delete(m.cache, sessionID)
	m.mu.Unlock()
	var errs []error
	for name, p := range scoped {
		if err := p.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// Close tears down every provider — global and session-scoped.
// Used on RuntimeCore.Shutdown to ensure no MCP subprocesses
// outlive the parent process. Idempotent.
func (m *ToolManager) Close() error {
	m.mu.Lock()
	globals := m.providers
	scoped := m.sessionProviders
	m.providers = make(map[string]ToolProvider)
	m.sessionProviders = make(map[string]map[string]ToolProvider)
	m.cache = make(map[string]*cachedSnapshot)
	m.mu.Unlock()
	var errs []error
	for name, p := range globals {
		if err := p.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close %s: %w", name, err))
		}
	}
	for sid, by := range scoped {
		for name, p := range by {
			if err := p.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close session %s/%s: %w", sid, name, err))
			}
		}
	}
	return errors.Join(errs...)
}

// AddProvider registers a ToolProvider. Constitution exception
// for plug-in registries (II.1).
func (m *ToolManager) AddProvider(p ToolProvider) error {
	if p == nil {
		return errors.New("tool: nil provider")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	name := p.Name()
	if name == "" {
		return errors.New("tool: provider with empty name")
	}
	if _, exists := m.providers[name]; exists {
		return fmt.Errorf("tool: provider %q already registered", name)
	}
	m.providers[name] = p
	m.toolGen.Add(1)
	m.invalidateAllSnapshots()
	return nil
}

// RemoveProvider drains in-flight calls (with the configured
// timeout) before disposing the provider. Calls in flight at
// remove time finish with ErrProviderRemoved if they reach the
// dispatch boundary after Close.
func (m *ToolManager) RemoveProvider(ctx context.Context, name string) error {
	m.mu.Lock()
	p, ok := m.providers[name]
	if !ok {
		m.mu.Unlock()
		return ErrUnknownProvider
	}
	delete(m.providers, name)
	m.toolGen.Add(1)
	m.invalidateAllSnapshots()
	m.mu.Unlock()

	// Drain: give in-flight calls a chance to complete by waiting
	// up to drainTimeout before Close. This is a coarse mechanism
	// — providers that need finer drain semantics should expose
	// their own Drain method on top of Close.
	dctx, cancel := context.WithTimeout(ctx, m.drainTimeout)
	defer cancel()
	<-dctx.Done()
	if err := p.Close(); err != nil {
		return fmt.Errorf("tool: close %s: %w", name, err)
	}
	return nil
}

// Providers returns the names of every registered provider.
func (m *ToolManager) Providers() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.providers))
	for n := range m.providers {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// Snapshot returns the per-Turn frozen catalogue for a session.
// The catalogue is cached per session keyed by Generations; a
// generation mismatch triggers a rebuild that filters every
// provider's tools by the session's loaded skills' allowed-tools.
func (m *ToolManager) Snapshot(ctx context.Context, sessionID string) (Snapshot, error) {
	gens, err := m.currentGenerations(ctx, sessionID)
	if err != nil {
		return Snapshot{}, err
	}
	m.mu.RLock()
	cached, ok := m.cache[sessionID]
	m.mu.RUnlock()
	if ok && cached.gens == gens {
		return cached.snap, nil
	}
	return m.rebuildSnapshot(ctx, sessionID, gens)
}

func (m *ToolManager) currentGenerations(ctx context.Context, sessionID string) (Generations, error) {
	gens := Generations{
		Tool:   m.toolGen.Load(),
		Policy: m.policyGen.Load(),
	}
	if m.skills != nil {
		b, err := m.skills.Bindings(ctx, sessionID)
		if err != nil {
			return gens, err
		}
		gens.Skill = b.Generation
	}
	return gens, nil
}

func (m *ToolManager) rebuildSnapshot(ctx context.Context, sessionID string, gens Generations) (Snapshot, error) {
	allowed := allowedFromBindings(ctx, m.skills, sessionID)

	m.mu.RLock()
	// Session-scoped providers shadow globals by name on collision —
	// a session can override "bash-mcp" with its own subprocess.
	merged := make(map[string]ToolProvider, len(m.providers))
	for n, p := range m.providers {
		merged[n] = p
	}
	for n, p := range m.sessionProviders[sessionID] {
		merged[n] = p
	}
	m.mu.RUnlock()

	var tools []Tool
	var errs []error
	for _, p := range merged {
		got, err := p.List(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.Name(), err))
			continue
		}
		for _, t := range got {
			if allowed != nil && !allowed[t.Name] {
				continue
			}
			tools = append(tools, t)
		}
	}
	slices.SortFunc(tools, func(a, b Tool) int { return strings.Compare(a.Name, b.Name) })

	snap := Snapshot{Generations: gens, Tools: tools}

	m.mu.Lock()
	m.cache[sessionID] = &cachedSnapshot{gens: gens, snap: snap}
	m.mu.Unlock()

	return snap, errors.Join(errs...)
}

func allowedFromBindings(ctx context.Context, skills *skill.SkillManager, sessionID string) map[string]bool {
	if skills == nil {
		return nil // no filter; expose every registered tool
	}
	b, err := skills.Bindings(ctx, sessionID)
	if err != nil || len(b.AllowedTools) == 0 {
		// No allowed-tools means no skills loaded — empty
		// catalogue. Distinguishes "no skills loaded" from "no
		// SkillManager configured" (the nil case above).
		return map[string]bool{}
	}
	out := map[string]bool{}
	for _, g := range b.AllowedTools {
		for _, t := range g.Tools {
			out[g.Provider+":"+t] = true
		}
	}
	return out
}

// Resolve gates a single tool call. Returns the merged
// Permission (after Tier-1 + Tier-2 — Tier-3 hook lands in T058)
// plus the effective args payload with template substitutions
// applied. Returns ErrPermissionDenied wrapped with the deciding
// tier on denial. Per-call session facts (SessionID,
// SessionMetadata) flow through ctx via perm.WithSession; the
// agent identity is captured at perm.Service construction.
func (m *ToolManager) Resolve(ctx context.Context, t Tool, args json.RawMessage) (perm.Permission, json.RawMessage, error) {
	p, err := m.perms.Resolve(ctx, t.PermissionObject, toolField(t.Name))
	if err != nil {
		return perm.Permission{}, nil, err
	}
	if p.Disabled {
		return p, nil, fmt.Errorf("%w: tier=%s",
			ErrPermissionDenied, deniedTier(p))
	}
	// Effective args = raw args merged with p.Data via shallow
	// JSON object merge (rule's Data wins on scalar conflict —
	// the LLM cannot override an operator-pinned arg).
	effective, err := mergeEffectiveArgs(args, p.Data)
	if err != nil {
		return p, nil, err
	}
	return p, effective, nil
}

// Dispatch executes a tool call. Args MUST be the substituted
// payload returned by Resolve, not the LLM's raw args. Session-
// scoped providers (via the SessionContext on ctx) shadow global
// providers of the same name on dispatch — used by per-session
// bash-mcp / python-mcp.
func (m *ToolManager) Dispatch(ctx context.Context, t Tool, effectiveArgs json.RawMessage) (json.RawMessage, error) {
	sc, _ := perm.SessionFromContext(ctx)
	m.mu.RLock()
	var p ToolProvider
	var ok bool
	if sc.SessionID != "" {
		if scoped, hasScope := m.sessionProviders[sc.SessionID]; hasScope {
			p, ok = scoped[t.Provider]
		}
	}
	if !ok {
		p, ok = m.providers[t.Provider]
	}
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownProvider, t.Provider)
	}
	short := toolField(t.Name)
	return p.Call(ctx, short, effectiveArgs)
}

// BumpPolicyGen bumps the policy generation counter so the next
// Snapshot rebuild picks up Tier-3 changes. Called by the
// Policies subsystem (T055) when policy_save / policy_revoke
// runs.
func (m *ToolManager) BumpPolicyGen() {
	m.policyGen.Add(1)
	m.invalidateAllSnapshots()
}

// invalidateAllSnapshots clears the per-session cache so the
// next Snapshot call rebuilds. Caller MUST hold m.mu (Lock — not
// RLock) when this runs alongside provider mutation; safe to
// call without the lock when it's the only mutation in flight.
func (m *ToolManager) invalidateAllSnapshots() {
	for k := range m.cache {
		delete(m.cache, k)
	}
}

// toolField extracts the field portion from a fully-qualified
// tool name "<provider>:<field>". When no colon is present the
// whole name is treated as the field; that matches the way
// system-tools providers expose unqualified names internally.
func toolField(fullName string) string {
	if i := strings.Index(fullName, ":"); i >= 0 {
		return fullName[i+1:]
	}
	return fullName
}

func deniedTier(p perm.Permission) string {
	switch {
	case p.FromUser:
		return "user"
	case p.FromRemote:
		return "remote"
	case p.FromConfig:
		return "config"
	default:
		return "unknown"
	}
}

func mergeEffectiveArgs(raw, ruleData json.RawMessage) (json.RawMessage, error) {
	if len(ruleData) == 0 {
		if len(raw) == 0 {
			return json.RawMessage(`{}`), nil
		}
		return raw, nil
	}
	var ruleObj map[string]any
	if err := json.Unmarshal(ruleData, &ruleObj); err != nil {
		return nil, fmt.Errorf("tool: rule data is not an object: %w", err)
	}
	var rawObj map[string]any
	if len(raw) == 0 || string(raw) == "null" {
		rawObj = map[string]any{}
	} else if err := json.Unmarshal(raw, &rawObj); err != nil {
		return nil, fmt.Errorf("tool: args is not a JSON object: %w", err)
	}
	for k, v := range ruleObj {
		// Rule wins.
		rawObj[k] = v
	}
	out, err := json.Marshal(rawObj)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(out), nil
}
