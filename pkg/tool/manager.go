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
	perms perm.Service
	// policies is the Tier-3 store. Lock-free read in Resolve via
	// atomic.Pointer — SetPolicies (and runtime_reload) can swap
	// the store concurrently with in-flight dispatches without
	// data-racing. nil pointer disables Tier-3 (IsConfigured is
	// nil-safe on the value side).
	policies atomic.Pointer[Policies]
	skills   *skill.SkillManager
	log      *slog.Logger

	providersView  config.ToolProvidersView
	authResolver   AuthResolver
	connectTimeout time.Duration

	builders map[string]ProviderBuilder

	mu               sync.RWMutex
	providers        map[string]ToolProvider
	sessionProviders map[string]map[string]ToolProvider // sessionID → name → provider
	cache            map[string]*cachedSnapshot
	cleanups         map[string][]func() // provider name → cleanup callbacks (e.g. revoke a minted secret)

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
	opts ...ToolManagerOption,
) *ToolManager {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	tm := &ToolManager{
		perms:            perms,
		skills:           skills,
		log:              log,
		providersView:    providers,
		authResolver:     resolver,
		connectTimeout:   defaultConnectTimeout,
		providers:        make(map[string]ToolProvider),
		sessionProviders: make(map[string]map[string]ToolProvider),
		cache:            make(map[string]*cachedSnapshot),
		cleanups:         make(map[string][]func()),
		drainTimeout:     defaultDrainTimeout,
	}
	for _, opt := range opts {
		opt(tm)
	}
	return tm
}

// ToolManagerOption configures optional dependencies on a
// freshly-built ToolManager. Variadic at the end of NewToolManager
// keeps the common case (no options) at the existing arity.
type ToolManagerOption func(*ToolManager)

// SetPolicies wires the Tier-3 store. Pass nil to disable Tier-3
// consultation (no-Hugr / no-local-DB deployments). Bumps the
// policy generation so the next Snapshot rebuild picks up the
// change. Safe to call any time — typical wiring is right after
// NewToolManager and before Init.
func (m *ToolManager) SetPolicies(p *Policies) {
	m.policies.Store(p)
	m.BumpPolicyGen()
}

// policiesSnapshot returns the current Tier-3 store pointer.
// nil-safe — callers can call IsConfigured on the result.
func (m *ToolManager) policiesSnapshot() *Policies {
	return m.policies.Load()
}

// WithProviderBuilder registers a builder for a non-MCP provider
// type. Init dispatches each tool_providers entry by `type` to its
// builder; entries with `type: mcp` (or empty) use the built-in
// MCP path. cmd/hugen registers `hugr-query`, `python-sandbox`
// etc. — runtime-managed kinds that need access to listener URL,
// agent token store, workspaces root, and similar facts pkg/tool
// has no business knowing.
//
// A builder may return cleanup callbacks alongside the provider;
// ToolManager runs them on RemoveProvider/Close. Use this for
// runtime-minted secrets that must be revoked when the provider
// goes away (e.g. agent_token bootstrap).
func WithProviderBuilder(typeName string, builder ProviderBuilder) ToolManagerOption {
	return func(tm *ToolManager) {
		if tm.builders == nil {
			tm.builders = make(map[string]ProviderBuilder)
		}
		tm.builders[strings.ToLower(typeName)] = builder
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
//
// The drain is a fixed-budget grace period (m.drainTimeout) — it
// always sleeps the full window before Close, regardless of how
// many calls are in flight. Providers that need synchronous "wait
// until last call returns" semantics should expose their own
// inflight wait-group on Close; the manager keeps the simple
// budget so a buggy provider can't block shutdown indefinitely.
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
	cleanups := m.cleanups[name]
	delete(m.cleanups, name)
	m.toolGen.Add(1)
	m.invalidateAllSnapshots()
	m.mu.Unlock()

	// Drain: sleep the full m.drainTimeout window before Close.
	// This is a fixed-budget grace period, NOT a synchronous wait
	// on inflight calls — the manager has no per-provider call
	// counter, so a clean drain depends on the provider letting
	// its in-flight handlers finish during the budget. Providers
	// that need precise "wait for last call to return" semantics
	// should expose their own inflight tracking and block in Close.
	dctx, cancel := context.WithTimeout(ctx, m.drainTimeout)
	defer cancel()
	<-dctx.Done()
	closeErr := p.Close()
	runCleanups(cleanups)
	if closeErr != nil {
		return fmt.Errorf("tool: close %s: %w", name, closeErr)
	}
	return nil
}

// recordCleanups associates teardown callbacks with a provider
// name so RemoveProvider/Close can run them.
func (m *ToolManager) recordCleanups(providerName string, cleanups []func()) {
	if len(cleanups) == 0 {
		return
	}
	m.mu.Lock()
	m.cleanups[providerName] = append(m.cleanups[providerName], cleanups...)
	m.mu.Unlock()
}

func runCleanups(fns []func()) {
	for _, fn := range fns {
		if fn != nil {
			fn()
		}
	}
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
			if allowed != nil && !allowed.match(t.Name) {
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

// allowedSet is the per-session compiled allow-list. Holds both
// exact names and `provider:prefix*` glob patterns so a skill
// granting `discovery-*` against the `hugr-main` provider matches
// every `hugr-main:discovery-<anything>` tool.
//
// nil ⇒ no filter (skill manager not wired in tests).
// empty (non-nil) ⇒ no skills loaded → empty catalogue.
type allowedSet struct {
	exact    map[string]bool
	patterns []string // each is "provider:prefix" with the trailing * stripped
}

// match reports whether the fully-qualified tool name (e.g.
// "hugr-main:discovery-search_data_sources") is allowed by any
// rule in the set.
func (a *allowedSet) match(name string) bool {
	if a == nil {
		return true
	}
	if a.exact[name] {
		return true
	}
	for _, p := range a.patterns {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func allowedFromBindings(ctx context.Context, skills *skill.SkillManager, sessionID string) *allowedSet {
	if skills == nil {
		return nil // no filter; expose every registered tool
	}
	b, err := skills.Bindings(ctx, sessionID)
	if err != nil || len(b.AllowedTools) == 0 {
		// No allowed-tools means no skills loaded — empty
		// catalogue. Distinguishes "no skills loaded" from "no
		// SkillManager configured" (the nil case above).
		return &allowedSet{exact: map[string]bool{}}
	}
	out := &allowedSet{exact: map[string]bool{}}
	for _, g := range b.AllowedTools {
		for _, t := range g.Tools {
			full := g.Provider + ":" + t
			if strings.HasSuffix(t, "*") {
				// glob pattern → prefix match. Strip the trailing
				// star; the prefix kept is "<provider>:<head>".
				out.patterns = append(out.patterns, strings.TrimSuffix(full, "*"))
				continue
			}
			out.exact[full] = true
		}
	}
	return out
}

// Resolve gates a single tool call. Returns the merged
// Permission (after Tier-1 + Tier-2 + Tier-3) plus the effective
// args payload with template substitutions applied. Returns
// ErrPermissionDenied wrapped with the deciding tier on denial.
// Per-call session facts (SessionID, SessionMetadata) flow
// through ctx via perm.WithSession; the agent identity is
// captured at perm.Service construction.
//
// Tier order:
//   - Tier 1 (operator config) and Tier 2 (Hugr role) merge inside
//     perm.Service.Resolve. A Disabled outcome there is final —
//     Tier 3 cannot relax the floor.
//   - Tier 3 (personal tool_policies) consults the Policies store
//     for the calling agent. PolicyAllow → run; PolicyDeny → block
//     with FromUser=true; PolicyAsk (or no row) → fall through to
//     the upstream decision.
func (m *ToolManager) Resolve(ctx context.Context, t Tool, args json.RawMessage) (perm.Permission, json.RawMessage, error) {
	p, err := m.perms.Resolve(ctx, t.PermissionObject, toolField(t.Name))
	if err != nil {
		return perm.Permission{}, nil, err
	}
	if p.Disabled {
		return p, nil, fmt.Errorf("%w: tier=%s",
			ErrPermissionDenied, deniedTier(p))
	}
	// Tier 3 — consult personal tool_policies. The agent id used
	// here is the policy owner (one row per (agent_id, tool_name,
	// scope)); LocalPermissions already cached it from
	// identity.Source on first Resolve. Until US4 ships
	// RemotePermissions with its own AgentID accessor we read
	// identity off the SessionContext for tests and fall back to
	// the perm.Service when available.
	if pol := m.policiesSnapshot(); pol.IsConfigured() {
		agentID, scope := tier3LookupKey(ctx, m.perms)
		dec, derr := pol.Decide(ctx, agentID, t.Name, scope)
		if derr != nil {
			return p, nil, derr
		}
		switch dec.Outcome {
		case PolicyDeny:
			p.FromUser = true
			return p, nil, fmt.Errorf("%w: tier=user", ErrPermissionDenied)
		case PolicyAllow:
			p.FromUser = true
		}
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

// tier3LookupKey extracts (agentID, scope) for the Tier-3
// lookup. AgentID comes from the perm.Service when it advertises
// the AgentID accessor (LocalPermissions and RemotePermissions
// both will, after US4); otherwise it falls back to the agent id
// pinned on the SessionContext metadata under the conventional
// "agent_id" key, and finally to the empty string. Scope is left
// at PolicyScopeGlobal in phase-3; skill/role scoping is a
// later refinement.
func tier3LookupKey(ctx context.Context, perms perm.Service) (string, string) {
	agentID := ""
	type agentIDer interface {
		AgentID() string
	}
	if a, ok := perms.(agentIDer); ok {
		agentID = a.AgentID()
	}
	if agentID == "" {
		if sc, ok := perm.SessionFromContext(ctx); ok {
			if sc.SessionMetadata != nil {
				agentID = sc.SessionMetadata["agent_id"]
			}
		}
	}
	return agentID, PolicyScopeGlobal
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
