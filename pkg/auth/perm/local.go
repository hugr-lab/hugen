package perm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/hugr-lab/hugen/pkg/auth/template"
	"github.com/hugr-lab/hugen/pkg/identity"
)

// LocalPermissions resolves permissions entirely from operator
// config (Tier 1). No Hugr round-trip on Resolve; Refresh is a
// no-op unless the underlying PermissionsView fires OnUpdate.
// AgentID and Role are sourced once from identity.Source
// (Agent + WhoAmI) and cached for the lifetime of the service —
// they're agent-stable and substituting them per call shouldn't
// pay a network round-trip. Tier-2 RemotePermissions overrides
// Role from the my_permissions snapshot in US4.
type LocalPermissions struct {
	cfg   PermissionsView
	ident identity.Source

	mu    sync.RWMutex
	rules []Rule

	identityOnce sync.Once
	agentID      string
	userID       string
	role         string

	gen            atomic.Int64
	subscribersMu  sync.Mutex
	subscribers    []chan RefreshEvent
	cancelOnUpdate func()
}

// Compile-time assertion.
var _ Service = (*LocalPermissions)(nil)

// NewLocalPermissions captures the current rule snapshot from cfg
// and binds the identity source used to resolve [$auth.user_id]
// / [$agent.id] template placeholders. Watches cfg.OnUpdate to
// re-snapshot and emit a RefreshEvent when the static service is
// replaced (phase-6+ live reload).
func NewLocalPermissions(cfg PermissionsView, ident identity.Source) *LocalPermissions {
	l := &LocalPermissions{cfg: cfg, ident: ident}
	l.snapshot()
	if cfg != nil {
		l.cancelOnUpdate = cfg.OnUpdate(func() {
			_ = l.Refresh(context.Background())
		})
	}
	return l
}

// Close cancels the cfg.OnUpdate subscription. Safe to omit when
// the parent context's cancellation already terminates the
// process; explicit Close is provided for tests.
func (l *LocalPermissions) Close() {
	if l.cancelOnUpdate != nil {
		l.cancelOnUpdate()
		l.cancelOnUpdate = nil
	}
}

func (l *LocalPermissions) snapshot() {
	if l.cfg == nil {
		return
	}
	r := l.cfg.Rules()
	l.mu.Lock()
	l.rules = r
	l.mu.Unlock()
}

// identityFacts is the cached (agent, user, role) triple resolved
// once from the bound identity.Source. AgentID is the agent's
// own runtime id; UserID and Role come from WhoAmI — in local
// mode both default to "local"; in remote/hub mode they reflect
// whoever the bound Hugr token represents (the agent's own Hugr
// identity, in autonomous deployments).
type identityFacts struct {
	AgentID, UserID, Role string
}

// AgentID returns the cached agent id resolved from the bound
// identity.Source. Returns the empty string if the source has not
// yet been consulted (i.e. before any Resolve call) or if the
// source declined to answer.
func (l *LocalPermissions) AgentID() string {
	l.resolveIdentity(context.Background())
	return l.agentID
}

// resolveIdentity lazily resolves the cached identity facts.
// Failures are swallowed — substitution returns empty strings,
// which surface as a clear (empty) value rather than failing
// every Resolve call.
func (l *LocalPermissions) resolveIdentity(ctx context.Context) identityFacts {
	l.identityOnce.Do(func() {
		if l.ident == nil {
			return
		}
		if a, err := l.ident.Agent(ctx); err == nil {
			l.agentID = a.ID
		}
		if w, err := l.ident.WhoAmI(ctx); err == nil {
			l.userID = w.UserID
			l.role = w.Role
		}
	})
	return identityFacts{AgentID: l.agentID, UserID: l.userID, Role: l.role}
}

// Resolve returns the merged Permission for (object, field). For
// LocalPermissions this is just the Tier-1 floor — no remote
// rules to layer on top.
func (l *LocalPermissions) Resolve(ctx context.Context, object, field string) (Permission, error) {
	if err := ctx.Err(); err != nil {
		return Permission{}, err
	}
	l.mu.RLock()
	rules := slices.Clone(l.rules)
	l.mu.RUnlock()

	tctx := l.templateContext(ctx)
	p, err := mergeRules(rules, tctx, object, field)
	if err != nil {
		return Permission{}, err
	}
	return p, nil
}

func (l *LocalPermissions) templateContext(ctx context.Context) template.Context {
	sc, _ := SessionFromContext(ctx)
	id := l.resolveIdentity(ctx)
	return template.Context{
		UserID:          id.UserID,
		AgentID:         id.AgentID,
		Role:            id.Role,
		SessionID:       sc.SessionID,
		SessionMetadata: sc.SessionMetadata,
	}
}

// Refresh re-reads the rule snapshot from cfg. A real change in
// the snapshot bumps the generation and emits a RefreshEvent.
// Static-service callers will not see a change but the call is
// safe and cheap.
func (l *LocalPermissions) Refresh(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if l.cfg == nil {
		return nil
	}
	prev := l.cfg.Rules()
	l.mu.Lock()
	l.rules = prev
	l.mu.Unlock()
	gen := l.gen.Add(1)
	l.broadcast(RefreshEvent{Generation: gen})
	return nil
}

// Subscribe streams RefreshEvent on every snapshot change.
func (l *LocalPermissions) Subscribe(ctx context.Context) (<-chan RefreshEvent, error) {
	ch := make(chan RefreshEvent, 4)
	l.subscribersMu.Lock()
	l.subscribers = append(l.subscribers, ch)
	l.subscribersMu.Unlock()
	go func() {
		<-ctx.Done()
		l.subscribersMu.Lock()
		idx := slices.Index(l.subscribers, ch)
		if idx >= 0 {
			l.subscribers = slices.Delete(l.subscribers, idx, idx+1)
		}
		l.subscribersMu.Unlock()
		close(ch)
	}()
	return ch, nil
}

func (l *LocalPermissions) broadcast(ev RefreshEvent) {
	l.subscribersMu.Lock()
	subs := slices.Clone(l.subscribers)
	l.subscribersMu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// mergeDataConfigWins merges two JSON objects with later (rule)
// values winning on scalar conflict. Both inputs must be JSON
// objects; arrays are replaced wholesale; nested objects do not
// recurse (shallow). Substitutes [$auth.*]/[$session.*] templates
// inside JSON string values via pkg/auth/template before merging.
func mergeDataConfigWins(prev, next json.RawMessage, tctx template.Context) (json.RawMessage, error) {
	nextSubst, err := template.Apply(next, tctx)
	if err != nil {
		return nil, fmt.Errorf("perm: data template: %w", err)
	}
	if len(prev) == 0 {
		return nextSubst, nil
	}
	var pm, nm map[string]any
	if err := json.Unmarshal(prev, &pm); err != nil {
		return nil, errors.New("perm: data merge: previous is not an object")
	}
	if err := json.Unmarshal(nextSubst, &nm); err != nil {
		return nil, errors.New("perm: data merge: rule is not an object")
	}
	for k, v := range nm {
		pm[k] = v
	}
	out, err := json.Marshal(pm)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(out), nil
}
