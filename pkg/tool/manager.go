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
	"github.com/hugr-lab/hugen/pkg/skill"
)

// ToolManager dispatches tool calls through the three permission
// tiers and the registered providers. One instance per agent;
// per-session catalogue is materialised via Snapshot.
type ToolManager struct {
	perms  perm.Service
	skills *skill.SkillManager
	log    *slog.Logger

	mu        sync.RWMutex
	providers map[string]ToolProvider
	cache     map[string]*cachedSnapshot

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

// Options groups optional knobs ToolManager constructors take.
type Options struct {
	Logger       *slog.Logger
	DrainTimeout time.Duration
}

// NewToolManager constructs the manager.
func NewToolManager(p perm.Service, skills *skill.SkillManager, opts Options) *ToolManager {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.DrainTimeout <= 0 {
		opts.DrainTimeout = 5 * time.Second
	}
	return &ToolManager{
		perms:        p,
		skills:       skills,
		log:          opts.Logger,
		providers:    make(map[string]ToolProvider),
		cache:        make(map[string]*cachedSnapshot),
		drainTimeout: opts.DrainTimeout,
	}
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
	provs := slices.Collect(maps_values(m.providers))
	m.mu.RUnlock()

	var tools []Tool
	var errs []error
	for _, p := range provs {
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

// maps_values is a tiny helper since we target Go 1.26 but the
// stdlib slices.Collect / maps.Values combo works cleaner in
// generic form here. Replace with maps.Values when the
// surrounding code is updated to use it directly.
func maps_values[K comparable, V any](m map[K]V) func(yield func(V) bool) {
	return func(yield func(V) bool) {
		for _, v := range m {
			if !yield(v) {
				return
			}
		}
	}
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
// tier on denial.
func (m *ToolManager) Resolve(ctx context.Context, ident Identity, t Tool, args json.RawMessage) (perm.Permission, json.RawMessage, error) {
	pIdent := perm.Identity{
		UserID:          ident.UserID,
		AgentID:         ident.AgentID,
		Role:            ident.Role,
		Roles:           ident.Roles,
		SessionID:       ident.SessionID,
		SessionMetadata: ident.SessionMetadata,
	}
	p, err := m.perms.Resolve(ctx, pIdent, t.PermissionObject, toolField(t.Name))
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
// payload returned by Resolve, not the LLM's raw args.
func (m *ToolManager) Dispatch(ctx context.Context, t Tool, effectiveArgs json.RawMessage) (json.RawMessage, error) {
	m.mu.RLock()
	p, ok := m.providers[t.Provider]
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
