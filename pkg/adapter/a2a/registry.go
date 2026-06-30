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
//
// The methods take NO ctx on purpose: a durable root must live for the whole
// adapter process, not a single request. hostRootStore opens/resumes with the
// adapter's long-lived lifecycle ctx — passing a per-request ctx would tie the
// session's Run loop to that request and kill it when the request returns
// (manager.Open starts the loop on the supplied ctx).
type rootStore interface {
	// openRoot opens a new durable root bound to contextID and returns its id.
	openRoot(contextID string) (rootID string, err error)
	// resumeRoot rehydrates an existing root by id; a non-nil error means it
	// is gone / not resumable (GC'd, terminated) and the caller opens fresh.
	resumeRoot(rootID string) error
	// boundRoot finds an active root already bound to contextID (the
	// restart-rebuild path). found=false when none exists.
	boundRoot(contextID string) (rootID string, found bool, err error)
}

// parkedInquiry is the adapter's record of an in-flight HITL inquiry that a
// turn surfaced to the client as an A2A `input-required` task. On the runtime
// side the inquiring tier is blocked inside session:inquire; the NEXT inbound
// on this contextId is the user's answer. Keying the park on the contextId (not
// the A2A task) is the design's robustness lever: it reconciles correctly even
// when a client like Copilot ignores stateful input-required and just replays
// with a fresh task. Mirrors tui.PendingInquiry. Spec §A5.
type parkedInquiry struct {
	RequestID       string
	CallerSessionID string
	Kind            string // protocol.InquiryType{Approval,Clarification,ResearchBatch}
	// Question is the rendered prompt text, retained so an unparseable answer
	// (e.g. an empty approval) can be re-asked against the same parked state.
	Question string
	// AsyncAwaited carries the async sub-agent session ids the parking Execute
	// was holding for (A6) when this inner inquiry fired — an inquiry raised
	// inside a running async mission. The Execute that answers the inquiry
	// restores them and resumes holding for their completion. Empty for an
	// ordinary (non-async) inquiry. Phase 8/A6.
	AsyncAwaited []string
}

// contextSession is the adapter's per-contextId view of a durable hugen root
// session. A2 holds the binding (contextId ↔ rootId); A5 adds the parked-inquiry
// state — set by the Execute that observes a session:inquire frame, read+cleared
// by the next Execute (the answer turn). No goroutine: the state lives here, and
// turns for one contextId run sequentially, but the mutex guards the rare
// concurrent-inbound race.
type contextSession struct {
	contextID string
	rootID    string

	mu      sync.Mutex
	pending *parkedInquiry
	// knownAsync is the global set of async sub-agent session ids the adapter
	// has already attributed to SOME request on this context (A6). It is the
	// diff baseline: when a turn-final frame reports the live async set, ids
	// already here belong to an earlier request, so only the genuinely-new ones
	// are assigned to the current turn's Task. First Execute to record an id
	// owns it; concurrent Tasks each hold their own local subset. Phase 8/A6.
	knownAsync map[string]struct{}
}

// RootID returns the durable root session id this context is bound to.
func (cs *contextSession) RootID() string { return cs.rootID }

// recordNewAsync records the supplied async refs as known and returns the
// session ids that were NOT previously known — i.e. the async sub-agents the
// current turn just launched (the caller's Task awaits these). Phase 8/A6.
func (cs *contextSession) recordNewAsync(refs []protocol.ActiveSubagentRef) []string {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.knownAsync == nil {
		cs.knownAsync = make(map[string]struct{})
	}
	var fresh []string
	for _, r := range refs {
		if r.SessionID == "" {
			continue
		}
		if _, ok := cs.knownAsync[r.SessionID]; ok {
			continue
		}
		cs.knownAsync[r.SessionID] = struct{}{}
		fresh = append(fresh, r.SessionID)
	}
	return fresh
}

// forgetAsync drops completed async session ids from the global known set
// (called by the owning Execute when their result lands). Phase 8/A6.
func (cs *contextSession) forgetAsync(ids []string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, id := range ids {
		delete(cs.knownAsync, id)
	}
}

// park records that this turn surfaced an inquiry; the next inbound is its
// answer. A fresh inquiry supersedes any stale pending one.
func (cs *contextSession) park(p *parkedInquiry) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.pending = p
}

// peekPending returns the in-flight inquiry without clearing it, so an
// unparseable answer can re-ask against the same parked state.
func (cs *contextSession) peekPending() *parkedInquiry {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.pending
}

// clearPending drops the in-flight inquiry once a valid answer was submitted.
func (cs *contextSession) clearPending() {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.pending = nil
}

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
// miss → resume an existing bound root (restart) or open a fresh one. No ctx
// param: the durable root lives on the adapter lifecycle, not the caller's
// request (see rootStore).
func (r *contextRegistry) resolve(contextID string) (*contextSession, error) {
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
	if rootID, found, err := r.store.boundRoot(contextID); err != nil {
		r.logger.Warn("a2a: boundRoot lookup failed; opening fresh", "context_id", contextID, "err", err)
	} else if found {
		if err := r.store.resumeRoot(rootID); err == nil {
			return r.bind(contextID, rootID, "resumed"), nil
		}
		r.logger.Warn("a2a: bound root not resumable; opening fresh", "context_id", contextID, "root", rootID)
	}

	rootID, err := r.store.openRoot(contextID)
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
//
// lifecycleCtx is the adapter's long-lived Run ctx (the whole `hugen a2a`
// process). It — NOT a per-request ctx — is what opens/resumes sessions, so a
// durable root's Run loop outlives any single A2A request (manager.Open starts
// the loop on the supplied ctx; a request ctx would kill the session on
// return).
type hostRootStore struct {
	host         adapter.Host
	owner        protocol.ParticipantInfo
	lifecycleCtx context.Context
}

var _ rootStore = hostRootStore{}

func (h hostRootStore) openRoot(contextID string) (string, error) {
	sess, _, err := h.host.OpenSession(h.lifecycleCtx, session.OpenRequest{
		OwnerID:      h.owner.ID,
		Participants: []protocol.ParticipantInfo{h.owner},
		Metadata:     map[string]any{contextIDMetaKey: contextID},
	})
	if err != nil {
		return "", err
	}
	return sess.ID(), nil
}

func (h hostRootStore) resumeRoot(rootID string) error {
	_, err := h.host.ResumeSession(h.lifecycleCtx, rootID)
	return err
}

func (h hostRootStore) boundRoot(contextID string) (string, bool, error) {
	// Only active sessions are resumable; a GC'd/terminated root must not be
	// returned (the caller would fail to resume and open fresh anyway). The
	// store lists newest-first, so the first metadata match is the freshest.
	sums, err := h.host.ListSessions(h.lifecycleCtx, store.StatusActive)
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
