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
)

// ToolManager dispatches tool calls through the three permission
// tiers and the registered providers. One instance per agent at
// the root; per-session children built via NewChild own their own
// providers map and walk to root for unknown-provider lookups.
//
// Per-session providers (bash-mcp, python-mcp, duckdb-mcp) live
// on the child Manager owned by pkg/session.Resources; child
// providers shadow root providers by name on collision.
type ToolManager struct {
	perms perm.Service
	// policies is the Tier-3 store. Lock-free read in Resolve via
	// atomic.Pointer — SetPolicies (and runtime_reload) can swap
	// the store concurrently with in-flight dispatches without
	// data-racing. nil pointer disables Tier-3 (IsConfigured is
	// nil-safe on the value side).
	policies atomic.Pointer[Policies]
	log      *slog.Logger

	// view is the per_agent provider catalogue. Init / LoadConfig
	// iterate it and dispatch through the wired ProviderBuilder.
	// nil disables config-driven loading (children, tests, deployments
	// without per_agent specs).
	view config.ToolProvidersView

	// builder is the Spec-driven dispatcher every AddBySpec call
	// goes through. Non-nil enables AddBySpec / LoadConfig; nil
	// disables them with ErrBuilderNotConfigured. Children inherit
	// the field from their parent so per_session AddBySpec routes
	// through the same dispatcher as per_agent.
	builder ProviderBuilder

	mu        sync.RWMutex
	providers map[string]ToolProvider

	toolGen   atomic.Int64
	policyGen atomic.Int64

	drainTimeout time.Duration

	// reconnector picks up MCPProviders that transition to stale
	// (after a synchronous maybeReconnect failure) and retries
	// connect with exponential backoff in the background. Created
	// by NewToolManager; Init starts its goroutine; Close stops it.
	// Nil-tolerant — providers without an MCP runtime keep working.
	reconnector *Reconnector

	// parent is non-nil on a child Manager (built via NewChild).
	// Child Managers walk to parent on provider-lookup miss so a
	// session can transparently see agent-level (root) providers
	// while owning its own per-session subset.
	parent *ToolManager
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

const defaultDrainTimeout = 5 * time.Second

// NewToolManager constructs the manager. Construction is cheap —
// no providers are connected here; call Init(ctx) when ready to
// start the background reconnector and load per_agent providers
// from the wired view (if any).
//
// Args:
//   - perms: permission service consulted on every Dispatch.
//   - view: per_agent MCP catalogue view. Read on Init().
//     Per_session entries are skipped (per-session children call
//     AddBySpec directly). Pass nil if no MCP entries are
//     configured (tests / deployments without config-driven
//     loading).
//   - log: structured logger; nil falls back to a discard handler.
//   - opts: WithBuilder wires the Spec-driven ProviderBuilder
//     AddBySpec dispatches through. Without it, AddBySpec /
//     LoadConfig surface ErrBuilderNotConfigured.
//
// Phase 4.1a stage A step 7d removed the auth.Service parameter —
// auth handling now lives inside providers.Builder, constructed
// at boot in pkg/runtime / cmd/hugen.
func NewToolManager(
	perms perm.Service,
	view config.ToolProvidersView,
	log *slog.Logger,
	opts ...ToolManagerOption,
) *ToolManager {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	tm := &ToolManager{
		perms:        perms,
		log:          log,
		view:         view,
		providers:    make(map[string]ToolProvider),
		drainTimeout: defaultDrainTimeout,
		reconnector:  NewReconnector(log),
	}
	for _, opt := range opts {
		opt(tm)
	}
	return tm
}

// Reconnector returns the manager's MCP background reconnector, used
// by callers (cmd/hugen) that want to wire OnRecover callbacks for
// session-level system_marker broadcasts. Children inherit via
// the parent reference — calling Reconnector() on a child walks
// to the root.
func (m *ToolManager) Reconnector() *Reconnector {
	if m.reconnector != nil {
		return m.reconnector
	}
	if m.parent != nil {
		return m.parent.Reconnector()
	}
	return nil
}

// NewChild builds a session-scoped Manager that inherits the
// agent-level dependency surface (perm.Service, skills,
// reconnector, log) and walks to its parent on provider-lookup
// miss. Each child owns its own providers map; children do NOT
// hold their own sessionProviders or cache state — those are
// agent-level concerns.
//
// The caller (today: pkg/session.Resources.Acquire — wired in
// stage A step 9 onward) is responsible for the child's lifecycle:
// child.AddProvider / child.AddBySpec to register per-session
// providers, child.Close on session release. Parent.Close()
// does NOT cascade into children — the parent has no registry of
// its children, only children hold a parent reference (one-way).
//
// On lookup, child.Dispatch / child.Resolve consults its own
// providers first; on miss the call walks to parent. Tier-3
// policies live on the root atomic.Pointer; child's
// policiesSnapshot walks to parent on nil load.
func (m *ToolManager) NewChild() *ToolManager {
	return &ToolManager{
		perms:        m.perms,
		log:          m.log,
		drainTimeout: m.drainTimeout,
		builder:      m.builder, // share dispatcher with root
		providers:    make(map[string]ToolProvider),
		parent:       m,
	}
}

// AddBySpec dispatches a tool.Spec through the wired
// ProviderBuilder (see WithBuilder) and registers the resulting
// provider. Returns ErrBuilderNotConfigured when no builder is
// wired — root Managers built before pkg/runtime injects one
// stay on the legacy Init path. Children inherit the builder
// from their parent.
//
// Failure inside Build does not register anything; the returned
// error is the Builder's verbatim. AddProvider failure (name
// collision) closes the freshly-built provider before returning.
func (m *ToolManager) AddBySpec(ctx context.Context, spec Spec) error {
	if m.builder == nil {
		return ErrBuilderNotConfigured
	}
	prov, err := m.builder.Build(ctx, spec)
	if err != nil {
		return err
	}
	if err := m.AddProvider(prov); err != nil {
		_ = prov.Close()
		return err
	}
	return nil
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
// nil-safe — callers can call IsConfigured on the result. On a
// child Manager whose own pointer is unset (children inherit
// rather than re-host), the call walks to parent so Tier-3
// gating uses the agent-level store.
func (m *ToolManager) policiesSnapshot() *Policies {
	if p := m.policies.Load(); p != nil {
		return p
	}
	if m.parent != nil {
		return m.parent.policiesSnapshot()
	}
	return nil
}

// WithBuilder pins the new Spec-driven ProviderBuilder consumed by
// AddBySpec. Boot wiring (pkg/runtime in stage B) constructs a
// providers.Builder and passes it via this option; AddBySpec on the
// resulting Manager (and on every child built from it) dispatches
// through the same instance. nil leaves AddBySpec disabled — calls
// surface ErrBuilderNotConfigured.
func WithBuilder(b ProviderBuilder) ToolManagerOption {
	return func(tm *ToolManager) {
		tm.builder = b
	}
}


// Close tears down the providers owned by this Manager. On the
// root Manager it stops the background reconnector. On a child
// Manager (parent != nil) it closes only the child's own
// providers — parent state is untouched and the reconnector
// belongs to the root. Idempotent on both paths.
//
// Phase 4.1a stage A step 9 retired the legacy sessionProviders
// map: per-session providers now live on a child ToolManager
// owned by pkg/session.Resources, and Resources.Release calls
// child.Close() to drop them.
func (m *ToolManager) Close() error {
	if m.parent == nil && m.reconnector != nil {
		m.reconnector.Stop()
	}
	m.mu.Lock()
	globals := m.providers
	m.providers = make(map[string]ToolProvider)
	m.mu.Unlock()
	var errs []error
	for name, p := range globals {
		if err := p.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// AddProvider registers a ToolProvider. Constitution exception
// for plug-in registries (II.1).
//
// MCPProviders get a stale-hook wired to the manager's Reconnector
// so a mid-flight EOF that fails an inline reconnect surfaces as a
// background retry instead of silently leaving the provider dead
// until process restart.
func (m *ToolManager) AddProvider(p ToolProvider) error {
	if p == nil {
		return errors.New("tool: nil provider")
	}
	m.mu.Lock()
	name := p.Name()
	if name == "" {
		m.mu.Unlock()
		return errors.New("tool: provider with empty name")
	}
	if _, exists := m.providers[name]; exists {
		m.mu.Unlock()
		return fmt.Errorf("tool: provider %q already registered", name)
	}
	m.providers[name] = p
	m.toolGen.Add(1)
	m.invalidateAllSnapshots()
	m.mu.Unlock()

	if mp, ok := p.(*MCPProvider); ok {
		if rec := m.Reconnector(); rec != nil {
			mp.SetStaleHook(rec.Track)
		}
	}
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
	if m.reconnector != nil {
		m.reconnector.Forget(name)
	}
	if err := p.Close(); err != nil {
		return fmt.Errorf("tool: close %s: %w", name, err)
	}
	return nil
}

// runCleanups runs a slice of teardown callbacks. Kept here as a
// helper because Init still calls it on the failure path before
// the provider is registered.
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

// ProviderLifetime returns the Lifetime declared by the provider
// registered under name. Walks parent on miss so children inherit
// agent-scoped providers from the root manager. Returns (0, false)
// when no provider is registered under that name on either tier.
func (m *ToolManager) ProviderLifetime(name string) (Lifetime, bool) {
	m.mu.RLock()
	if p, ok := m.providers[name]; ok {
		m.mu.RUnlock()
		return p.Lifetime(), true
	}
	parent := m.parent
	m.mu.RUnlock()
	if parent != nil {
		return parent.ProviderLifetime(name)
	}
	return 0, false
}

// Snapshot returns the per-Turn frozen catalogue for a session.
// The catalogue is cached per session keyed by Generations; a
// generation mismatch in the caller's cache triggers a rebuild
// that filters every provider's tools by the session's loaded
// skills' allowed-tools. Phase 4.1a stage A step 7b moved the
// per-session caching to the caller (pkg/session) — Manager's
// Snapshot rebuilds every call. Generations come back keyed for
// caller cache invalidation.
func (m *ToolManager) Snapshot(ctx context.Context, sessionID string) (Snapshot, error) {
	gens, err := m.currentGenerations(ctx, sessionID)
	if err != nil {
		return Snapshot{}, err
	}
	return m.rebuildSnapshot(ctx, sessionID, gens)
}

// ToolGen exposes the current tool generation. Bumped on
// AddProvider / RemoveProvider; used by per-session snapshot
// caches in pkg/session to detect when a rebuild is needed.
func (m *ToolManager) ToolGen() int64 { return m.toolGen.Load() }

// PolicyGen exposes the current policy generation. Bumped on
// SetPolicies / BumpPolicyGen; same role as ToolGen for the
// caller's cache invalidation logic.
func (m *ToolManager) PolicyGen() int64 { return m.policyGen.Load() }

func (m *ToolManager) currentGenerations(_ context.Context, _ string) (Generations, error) {
	// Phase 4.1a stage A step 7c moved skill-bindings reading to
	// pkg/session — Manager only owns Tool / Policy gens. The
	// Skill field stays on the struct for caller-side cache keys
	// (pkg/session populates it) but Manager always returns 0.
	return Generations{
		Tool:   m.toolGen.Load(),
		Policy: m.policyGen.Load(),
	}, nil
}

func (m *ToolManager) rebuildSnapshot(ctx context.Context, sessionID string, gens Generations) (Snapshot, error) {
	merged := m.collectMergedProviders(sessionID)

	var tools []Tool
	var errs []error
	for _, p := range merged {
		got, err := p.List(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.Name(), err))
			continue
		}
		tools = append(tools, got...)
	}
	slices.SortFunc(tools, func(a, b Tool) int { return strings.Compare(a.Name, b.Name) })

	return Snapshot{Generations: gens, Tools: tools}, errors.Join(errs...)
}

// collectMergedProviders returns the effective provider set
// visible from this Manager: own providers + (legacy)
// sessionProviders[sessionID] + parent's set walked recursively.
// Child shadows parent on name collision (per spec §6.4a) — a
// session can override a global provider by registering its own
// under the same name on its child Manager.
func (m *ToolManager) collectMergedProviders(sessionID string) map[string]ToolProvider {
	merged := map[string]ToolProvider{}
	if m.parent != nil {
		for n, p := range m.parent.collectMergedProviders(sessionID) {
			merged[n] = p
		}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for n, p := range m.providers {
		merged[n] = p
	}
	return merged
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
// payload returned by Resolve, not the LLM's raw args. On a
// child Manager (built via NewChild), Dispatch consults the
// child's own providers first and walks to parent on miss — the
// per_session providers shadow root providers by name.
func (m *ToolManager) Dispatch(ctx context.Context, t Tool, effectiveArgs json.RawMessage) (json.RawMessage, error) {
	m.mu.RLock()
	p, ok := m.providers[t.Provider]
	m.mu.RUnlock()
	if !ok {
		if m.parent != nil {
			return m.parent.Dispatch(ctx, t, effectiveArgs)
		}
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

// invalidateAllSnapshots is a no-op since stage A step 7b moved
// snapshot caching to the caller (pkg/session). The function
// stays as a named call site so AddProvider / RemoveProvider /
// BumpPolicyGen documentation continues to read coherently —
// callers consult ToolGen() / PolicyGen() directly to detect
// invalidation. Removed in step 9 alongside the legacy
// session-providers surface.
func (m *ToolManager) invalidateAllSnapshots() {}

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
