package a2a

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/hugr-lab/hugen/pkg/adapter"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// contextIDMetaKey is the session-metadata key under which the adapter
// records the A2A contextId that a durable root session is bound to. It is
// persisted verbatim on the session row (OpenRequest.Metadata) and read back
// from SessionSummary.Metadata on the restart-rebuild path — so the binding
// survives a process bounce without a dedicated table (spec §A2).
const contextIDMetaKey = "a2a_context_id"

// errEmptyContextID is returned by contextRegistry.resolve for a blank id.
var errEmptyContextID = errors.New("a2a: empty contextID")

// rootStore is the narrow session-lifecycle surface the contextRegistry
// depends on. It is deliberately ID-based (no *session.Session) so the
// registry is unit-testable with a trivial fake — fabricating a real
// *session.Session needs a full session manager. adapter.Host satisfies it
// via hostRootStore; A3's executor uses adapter.Host directly for the
// Subscribe/Submit traffic, which is out of this resolver's scope.
type rootStore interface {
	// openRoot opens a new durable root bound to contextID and returns its id.
	openRoot(ctx context.Context, contextID string) (rootID string, err error)
	// resumeRoot rehydrates an existing root by id; a non-nil error means it
	// is gone / not resumable (GC'd, terminated) and the caller opens fresh.
	resumeRoot(ctx context.Context, rootID string) error
	// boundRoot finds an active root already bound to contextID (the
	// restart-rebuild path). found=false when none exists.
	boundRoot(ctx context.Context, contextID string) (rootID string, found bool, err error)
}

// contextSession is the adapter's per-contextId view of a durable hugen root
// session. A2 holds only the binding (contextId ↔ rootId). A3 grows it with
// the long-lived Subscribe channel, the reader goroutine, and the observed
// session state (parked? pending inquiry? active long-task).
type contextSession struct {
	contextID string
	rootID    string
}

// RootID returns the durable root session id this context is bound to.
func (cs *contextSession) RootID() string { return cs.rootID }

// contextRegistry maps A2A contextIds to durable root sessions, opening or
// resuming as needed. One ContextSession per contextId; safe for concurrent
// use. The mutex is held across the store calls on a cache miss so two
// concurrent inbounds for the same *new* contextId cannot open two roots —
// correctness over throughput for the single-tenant v1.
type contextRegistry struct {
	store  rootStore
	logger *slog.Logger

	mu        sync.Mutex
	byContext map[string]*contextSession
}

func newContextRegistry(rs rootStore, logger *slog.Logger) *contextRegistry {
	if logger == nil {
		logger = slog.Default()
	}
	return &contextRegistry{
		store:     rs,
		logger:    logger,
		byContext: make(map[string]*contextSession),
	}
}

// resolve returns the contextSession bound to contextID. Cache hit → reuse;
// miss → resume an existing bound root (restart) or open a fresh one.
func (r *contextRegistry) resolve(ctx context.Context, contextID string) (*contextSession, error) {
	if contextID == "" {
		return nil, errEmptyContextID
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if cs, ok := r.byContext[contextID]; ok {
		return cs, nil
	}

	// Cache miss. After a restart the in-memory map is empty but a durable
	// root may still carry the binding in its metadata — find + resume it.
	if rootID, found, err := r.store.boundRoot(ctx, contextID); err != nil {
		r.logger.Warn("a2a: boundRoot lookup failed; opening fresh", "context_id", contextID, "err", err)
	} else if found {
		if err := r.store.resumeRoot(ctx, rootID); err == nil {
			return r.bind(contextID, rootID, "resumed"), nil
		}
		r.logger.Warn("a2a: bound root not resumable; opening fresh", "context_id", contextID, "root", rootID)
	}

	rootID, err := r.store.openRoot(ctx, contextID)
	if err != nil {
		return nil, fmt.Errorf("a2a: open root for context %q: %w", contextID, err)
	}
	return r.bind(contextID, rootID, "opened"), nil
}

// bind records the contextId↔rootId mapping in the in-memory cache. Caller
// holds r.mu.
func (r *contextRegistry) bind(contextID, rootID, how string) *contextSession {
	cs := &contextSession{contextID: contextID, rootID: rootID}
	r.byContext[contextID] = cs
	r.logger.Info("a2a: durable root "+how+" for context", "context_id", contextID, "root", rootID)
	return cs
}

// forget drops the in-memory binding (used by idle-GC, A8). The durable
// session and its metadata are untouched; a later inbound on the same
// contextId rebuilds the binding lazily via boundRoot/resumeRoot.
func (r *contextRegistry) forget(contextID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byContext, contextID)
}

// hostRootStore is the production rootStore: it drives the runtime via
// adapter.Host, stamping the contextId binding into session metadata on open
// and reading it back from session summaries on rebuild.
type hostRootStore struct {
	host  adapter.Host
	owner protocol.ParticipantInfo
}

var _ rootStore = hostRootStore{}

func (h hostRootStore) openRoot(ctx context.Context, contextID string) (string, error) {
	sess, _, err := h.host.OpenSession(ctx, session.OpenRequest{
		OwnerID:      h.owner.ID,
		Participants: []protocol.ParticipantInfo{h.owner},
		Metadata:     map[string]any{contextIDMetaKey: contextID},
	})
	if err != nil {
		return "", err
	}
	return sess.ID(), nil
}

func (h hostRootStore) resumeRoot(ctx context.Context, rootID string) error {
	_, err := h.host.ResumeSession(ctx, rootID)
	return err
}

func (h hostRootStore) boundRoot(ctx context.Context, contextID string) (string, bool, error) {
	// Only active sessions are resumable; a GC'd/terminated root must not be
	// returned (the caller would fail to resume and open fresh anyway). The
	// store lists newest-first, so the first metadata match is the freshest.
	sums, err := h.host.ListSessions(ctx, store.StatusActive)
	if err != nil {
		return "", false, err
	}
	for _, s := range sums {
		if metaString(s.Metadata, contextIDMetaKey) == contextID {
			return s.ID, true, nil
		}
	}
	return "", false, nil
}

// metaString reads a string value out of a session-metadata map, tolerating
// the any-typed value that survives JSON persistence + restore.
func metaString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// serviceParticipant is the single service identity that owns A2A-opened
// root sessions in v1 (no per-end-user identity — Copilot forwards a service
// credential, not the Teams user; spec §A9 / design risk 2). Auth (A9) may
// override the id/name later.
func serviceParticipant() protocol.ParticipantInfo {
	return protocol.ParticipantInfo{ID: "a2a-service", Kind: protocol.ParticipantUser, Name: "a2a"}
}
