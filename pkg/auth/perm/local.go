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
)

// LocalPermissions resolves permissions entirely from operator
// config (Tier 1). No Hugr round-trip; Refresh is a no-op unless
// the underlying PermissionsView fires OnUpdate.
type LocalPermissions struct {
	cfg PermissionsView

	mu    sync.RWMutex
	rules []Rule

	gen           atomic.Int64
	subscribersMu sync.Mutex
	subscribers   []chan RefreshEvent
	cancelOnUpdate func()
}

// Compile-time assertion.
var _ Service = (*LocalPermissions)(nil)

// NewLocalPermissions captures the current rule snapshot from
// cfg. Watches cfg.OnUpdate to re-snapshot and emit a
// RefreshEvent when the static service is replaced (phase-6+
// live reload). For phase-3's static service this never fires.
func NewLocalPermissions(cfg PermissionsView) *LocalPermissions {
	l := &LocalPermissions{cfg: cfg}
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

// Resolve returns the merged Permission for (object, field). For
// LocalPermissions this is just the Tier-1 floor — no remote
// rules to layer on top.
func (l *LocalPermissions) Resolve(ctx context.Context, ident Identity, object, field string) (Permission, error) {
	if err := ctx.Err(); err != nil {
		return Permission{}, err
	}
	l.mu.RLock()
	rules := slices.Clone(l.rules)
	l.mu.RUnlock()

	p, err := mergeConfig(rules, ident, object, field)
	if err != nil {
		return Permission{}, err
	}
	return p, nil
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

// mergeConfig produces the Tier-1 Permission for (object, field)
// against a rule list. Wildcard `*` rules contribute to the
// merge; exact-field rules override on scalar conflict (more
// specific wins inside the same tier).
func mergeConfig(rules []Rule, ident Identity, object, field string) (Permission, error) {
	out := Permission{}
	matched := false
	tctx := ident.TemplateContext()

	// Wildcards first, then exact-field rules — that way
	// exact-field scalar values win on conflict (config-wins
	// inside the same tier).
	apply := func(r Rule) error {
		if r.Type != object {
			return nil
		}
		if r.Field != "*" && r.Field != field {
			return nil
		}
		if r.Disabled {
			out.Disabled = true
		}
		if r.Hidden {
			out.Hidden = true
		}
		if len(r.Data) > 0 {
			merged, err := mergeDataConfigWins(out.Data, r.Data, tctx)
			if err != nil {
				return err
			}
			out.Data = merged
		}
		if r.Filter != "" {
			f := template.ApplyString(r.Filter, tctx)
			if out.Filter == "" {
				out.Filter = f
			} else {
				out.Filter = "(" + out.Filter + ") AND (" + f + ")"
			}
		}
		matched = true
		return nil
	}
	for _, r := range rules {
		if r.Field == "*" {
			if err := apply(r); err != nil {
				return Permission{}, err
			}
		}
	}
	for _, r := range rules {
		if r.Field != "*" {
			if err := apply(r); err != nil {
				return Permission{}, err
			}
		}
	}
	if matched {
		out.FromConfig = true
	}
	return out, nil
}

// mergeDataConfigWins merges two JSON objects with later (config
// rule) values winning on scalar conflict. Both inputs must be
// JSON objects; arrays are replaced wholesale; nested objects
// recurse. Substitutes [$auth.*]/[$session.*] templates inside
// JSON string values via pkg/auth/template.
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
		// next (this rule) wins on conflict — config-wins inside
		// Tier 1, and Tier 2's RemotePermissions will treat the
		// Tier-1 result as `prev` and itself as `next` only when
		// remote-wins; phase 3 ships LocalPermissions so this
		// path is exercised here.
		pm[k] = v
	}
	out, err := json.Marshal(pm)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(out), nil
}
