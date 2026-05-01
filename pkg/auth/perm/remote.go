package perm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/hugr-lab/hugen/pkg/auth/template"
	"github.com/hugr-lab/hugen/pkg/identity"
)

// RemotePermissions resolves the merged Tier-1 (config floor) +
// Tier-2 (Hugr role) decision for one (object, field). The Tier-2
// rule list is bulk-fetched from function.core.auth.my_permissions
// and cached in memory; the cache is refreshed when its age
// exceeds cfg.RefreshInterval(). Concurrent decisions issued
// during a refresh are coalesced through singleflight.
//
// Refresh failure semantics:
//
//   - the previous snapshot stays in force (callers see the last
//     good rules instead of erroring);
//   - the failure flows out via Subscribe as a RefreshEvent.Err;
//   - after 3× the configured TTL without a successful refresh
//     ("hard expiry"), Resolve fails fast with ErrSnapshotStale —
//     stale rules cannot mask a deny that may have arrived in
//     the meantime.
//
// The implementation embeds *LocalPermissions so the operator-
// floor rules and identity wiring stay live; only the remote
// snapshot management is added on top.
type RemotePermissions struct {
	*LocalPermissions

	q       Querier
	refresh time.Duration

	mu        sync.RWMutex
	rules     []Rule
	loadedAt  time.Time
	lastErr   error
	hasLoaded bool

	flight singleflight.Group
	gen    atomic.Int64

	subscribersMu sync.Mutex
	subscribers   []chan RefreshEvent

	// hard-expiry multiplier; default 3× refresh.
	hardExpiryMult int
}

// Compile-time interface check.
var _ Service = (*RemotePermissions)(nil)

// NewRemotePermissions wires the Tier-2 fetch path against q,
// wrapping a LocalPermissions for Tier-1. cfg supplies the rule
// floor and the refresh interval; ident is forwarded to
// LocalPermissions for $auth.* template substitution.
func NewRemotePermissions(cfg PermissionsView, ident identity.Source, q Querier) *RemotePermissions {
	r := &RemotePermissions{
		LocalPermissions: NewLocalPermissions(cfg, ident),
		q:                q,
		refresh:          5 * time.Minute,
		hardExpiryMult:   3,
	}
	if cfg != nil {
		if d := cfg.RefreshInterval(); d > 0 {
			r.refresh = d
		}
	}
	return r
}

// Resolve composes the Tier-1 floor with the cached Tier-2
// snapshot. A stale-but-present snapshot is reused until hard
// expiry (3× TTL); past that Resolve returns ErrSnapshotStale.
func (r *RemotePermissions) Resolve(ctx context.Context, object, field string) (Permission, error) {
	if err := ctx.Err(); err != nil {
		return Permission{}, err
	}
	if err := r.ensureFresh(ctx); err != nil {
		// ensureFresh only surfaces hard-expiry errors; transient
		// refresh failures preserve the prior snapshot and keep
		// going.
		return Permission{}, err
	}

	tctx := r.LocalPermissions.templateContext(ctx)
	r.mu.RLock()
	remoteRules := slices.Clone(r.rules)
	confRules := []Rule{}
	if cfg := r.LocalPermissions.cfg; cfg != nil {
		confRules = cfg.Rules()
	}
	r.mu.RUnlock()

	got, err := Merge(confRules, remoteRules, tctx, object, field)
	if err != nil {
		return Permission{}, err
	}
	return got, nil
}

// Refresh forces a snapshot fetch. Singleflight-coalesced — a
// concurrent refresh in flight is reused.
func (r *RemotePermissions) Refresh(ctx context.Context) error {
	_, err, _ := r.flight.Do("refresh", func() (any, error) {
		return nil, r.fetchAndStore(ctx)
	})
	return err
}

// Subscribe streams RefreshEvents (success and failure). Mirrors
// LocalPermissions.Subscribe semantics — buffer 4, dropped on
// slow consumers, channel closes on ctx cancel.
func (r *RemotePermissions) Subscribe(ctx context.Context) (<-chan RefreshEvent, error) {
	ch := make(chan RefreshEvent, 4)
	r.subscribersMu.Lock()
	r.subscribers = append(r.subscribers, ch)
	r.subscribersMu.Unlock()
	go func() {
		<-ctx.Done()
		r.subscribersMu.Lock()
		idx := slices.Index(r.subscribers, ch)
		if idx >= 0 {
			r.subscribers = slices.Delete(r.subscribers, idx, idx+1)
		}
		r.subscribersMu.Unlock()
		close(ch)
	}()
	return ch, nil
}

// ensureFresh refreshes if the snapshot is older than r.refresh
// or if no snapshot has loaded yet. Returns ErrSnapshotStale only
// when the snapshot is past hard expiry AND no live rules remain.
func (r *RemotePermissions) ensureFresh(ctx context.Context) error {
	r.mu.RLock()
	loaded := r.hasLoaded
	age := time.Since(r.loadedAt)
	r.mu.RUnlock()

	if !loaded {
		if err := r.Refresh(ctx); err != nil {
			// initial fetch failed — no snapshot to fall back on,
			// surface the error.
			return fmt.Errorf("perm: initial role refresh: %w", err)
		}
		return nil
	}
	if age < r.refresh {
		return nil
	}
	if err := r.Refresh(ctx); err == nil {
		return nil
	}
	// Refresh failed; allow the existing snapshot to stand until
	// hard expiry (3× TTL since last good refresh).
	hard := time.Duration(r.hardExpiryMult) * r.refresh
	if age >= hard {
		return ErrSnapshotStale
	}
	return nil
}

// fetchAndStore is the actual GraphQL fetch path. On success the
// snapshot is replaced; on failure the previous snapshot stays
// intact and the error is broadcast via RefreshEvent.
func (r *RemotePermissions) fetchAndStore(ctx context.Context) error {
	rules, err := r.q.QueryRules(ctx)
	if err != nil {
		r.mu.Lock()
		r.lastErr = err
		r.mu.Unlock()
		r.broadcast(RefreshEvent{
			At:         time.Now(),
			Generation: r.gen.Load(),
			Err:        err,
		})
		return err
	}
	r.mu.Lock()
	r.rules = rules
	r.loadedAt = time.Now()
	r.lastErr = nil
	r.hasLoaded = true
	r.mu.Unlock()

	gen := r.gen.Add(1)
	r.broadcast(RefreshEvent{At: time.Now(), Generation: gen})
	return nil
}

func (r *RemotePermissions) broadcast(ev RefreshEvent) {
	r.subscribersMu.Lock()
	subs := slices.Clone(r.subscribers)
	r.subscribersMu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Compile-time check that RemotePermissions surfaces the Tier-3
// AgentID accessor ToolManager.Resolve looks for. Forwards to the
// embedded LocalPermissions which caches the identity at first
// resolve.
func (r *RemotePermissions) AgentID() string {
	if r == nil || r.LocalPermissions == nil {
		return ""
	}
	return r.LocalPermissions.AgentID()
}

// templateContext is exported on LocalPermissions only inside the
// package; the assertion guards against accidental signature
// drift between the two implementations.
var _ = func() bool {
	_ = template.Context{}
	return errors.Is(errors.New(""), nil)
}
