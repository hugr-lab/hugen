package http

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// httpHostFakePerms allows everything; only used to satisfy the
// tool.ToolManager constructor in the host fixture.
type httpHostFakePerms struct{}

func (httpHostFakePerms) Resolve(_ context.Context, _, _ string) (perm.Permission, error) {
	return perm.Permission{}, nil
}
func (httpHostFakePerms) Refresh(_ context.Context) error { return nil }
func (httpHostFakePerms) Subscribe(_ context.Context) (<-chan perm.RefreshEvent, error) {
	return nil, nil
}

// stubIdentity is the minimal identity.Source the tests need to feed
// session.NewAgent (which rejects nil sources).
type stubIdentity struct{}

func (stubIdentity) Agent(_ context.Context) (identity.Agent, error) {
	return identity.Agent{ID: "agent-test", Name: "hugen-test"}, nil
}
func (stubIdentity) WhoAmI(_ context.Context) (identity.WhoAmI, error) {
	return identity.WhoAmI{UserID: "agent-test"}, nil
}
func (stubIdentity) Permission(_ context.Context, _, _ string) (identity.Permission, error) {
	return identity.Permission{Enabled: true}, nil
}

func newTestAgent() *session.Agent {
	a, err := session.NewAgent("agent-test", "hugen-test", stubIdentity{}, "", nil)
	if err != nil {
		panic(err)
	}
	return a
}

// fakeHost is a minimal in-memory session.AdapterHost suitable for
// HTTP-adapter tests: no DuckDB, no real session goroutine, just
// enough to exercise handler shapes, SSE framing, slow-consumer
// behaviour, and reconnection replay.
type fakeHost struct {
	mu        sync.Mutex
	logger    *slog.Logger
	agent     *session.Agent
	store     *fakeStore
	sessions  map[string]*session.SessionRow
	subs      map[string][]chan protocol.Frame
	openErr   error
	submitErr error
	subErr    error
	closeErr  error
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		logger:   slog.Default(),
		agent:    newTestAgent(),
		store:    newFakeStore(),
		sessions: map[string]*session.SessionRow{},
		subs:     map[string][]chan protocol.Frame{},
	}
}

// Logger satisfies session.AdapterHost.
func (f *fakeHost) Logger() *slog.Logger { return f.logger }

func (f *fakeHost) OpenSession(_ context.Context, req session.OpenRequest) (*session.Session, time.Time, error) {
	if f.openErr != nil {
		return nil, time.Time{}, f.openErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := "ses-test-" + nextSessionSuffix()
	now := time.Now().UTC()
	f.sessions[id] = &session.SessionRow{
		ID:        id,
		Status:    session.StatusActive,
		Metadata:  req.Metadata,
		CreatedAt: now,
		UpdatedAt: now,
	}
	// fakeHost returns a non-nil *session.Session so handlers can
	// call Session.ID(). The session goroutine isn't running; only
	// the public id surface is read by handlers.
	return session.NewSession(id, f.agent, nil, nil, nil, nil, tool.NewToolManager(httpHostFakePerms{}, nil, nil), f.logger), now, nil
}

func (f *fakeHost) ResumeSession(_ context.Context, id string) (*session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sessions[id]; !ok {
		return nil, session.ErrSessionNotFound
	}
	return session.NewSession(id, f.agent, nil, nil, nil, nil, tool.NewToolManager(httpHostFakePerms{}, nil, nil), f.logger), nil
}

func (f *fakeHost) Submit(_ context.Context, frame protocol.Frame) error {
	if f.submitErr != nil {
		return f.submitErr
	}
	f.mu.Lock()
	subs := append([]chan protocol.Frame(nil), f.subs[frame.SessionID()]...)
	f.mu.Unlock()
	for _, c := range subs {
		select {
		case c <- frame:
		default:
		}
	}
	return nil
}

func (f *fakeHost) Subscribe(ctx context.Context, sessionID string) (<-chan protocol.Frame, error) {
	if f.subErr != nil {
		return nil, f.subErr
	}
	c := make(chan protocol.Frame, 64)
	f.mu.Lock()
	f.subs[sessionID] = append(f.subs[sessionID], c)
	f.mu.Unlock()
	go func() {
		<-ctx.Done()
		f.mu.Lock()
		defer f.mu.Unlock()
		out := f.subs[sessionID][:0]
		for _, sub := range f.subs[sessionID] {
			if sub != c {
				out = append(out, sub)
			}
		}
		f.subs[sessionID] = out
	}()
	return c, nil
}

func (f *fakeHost) CloseSession(_ context.Context, id, _ string) (time.Time, error) {
	if f.closeErr != nil {
		return time.Time{}, f.closeErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.sessions[id]
	if !ok {
		return time.Time{}, session.ErrSessionNotFound
	}
	if row.Status == session.StatusTerminated {
		return row.UpdatedAt, nil
	}
	row.Status = session.StatusTerminated
	row.UpdatedAt = time.Now().UTC()
	return row.UpdatedAt, nil
}

func (f *fakeHost) ListSessions(_ context.Context, status string) ([]session.SessionSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]session.SessionSummary, 0, len(f.sessions))
	for _, row := range f.sessions {
		if status != "" && row.Status != status {
			continue
		}
		out = append(out, session.SessionSummary{
			ID:        row.ID,
			Status:    row.Status,
			OpenedAt:  row.CreatedAt,
			UpdatedAt: row.UpdatedAt,
			Metadata:  row.Metadata,
		})
	}
	return out, nil
}

func (f *fakeHost) ListEvents(_ context.Context, _ string, _ session.ListEventsOpts) ([]session.EventRow, error) {
	// HTTP adapter tests don't exercise replay; the existing
	// fakeStore is the ReplaySource used by the reconnect path.
	// Return empty so the AdapterHost interface is satisfied.
	return nil, nil
}

func (f *fakeHost) SessionStats(_ context.Context, _ string) (int, error) { return 0, nil }

// publish routes a frame to every subscriber of sessionID; tests use
// it to drive the SSE writer.
func (f *fakeHost) publish(sessionID string, frame protocol.Frame) {
	f.mu.Lock()
	subs := append([]chan protocol.Frame(nil), f.subs[sessionID]...)
	f.mu.Unlock()
	for _, c := range subs {
		c <- frame
	}
}

// fakeStore is a slice-backed ReplaySource for reconnection tests.
type fakeStore struct {
	mu     sync.Mutex
	events map[string][]session.EventRow
	err    error
}

func newFakeStore() *fakeStore {
	return &fakeStore{events: map[string][]session.EventRow{}}
}

func (s *fakeStore) ListEvents(_ context.Context, sessionID string, opts session.ListEventsOpts) ([]session.EventRow, error) {
	if s.err != nil {
		return nil, s.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := s.events[sessionID]
	out := make([]session.EventRow, 0, len(rows))
	for _, r := range rows {
		if opts.MinSeq > 0 && r.Seq <= opts.MinSeq {
			continue
		}
		out = append(out, r)
	}
	if opts.Limit > 0 && len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, nil
}

func (s *fakeStore) appendEvent(sessionID string, ev session.EventRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events[sessionID] = append(s.events[sessionID], ev)
}

// nextSessionSuffix generates a short suffix unique within a single
// process; tests don't need cryptographic guarantees.
var (
	suffixMu sync.Mutex
	suffixN  int
)

func nextSessionSuffix() string {
	suffixMu.Lock()
	defer suffixMu.Unlock()
	suffixN++
	return time.Now().Format("150405") + "-" + itoa(suffixN)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// allowAllAuth passes every token. Tests that exercise the auth gate
// use a concrete *DevTokenStore; tests that exercise non-auth paths
// short-circuit auth so a missing Authorization header doesn't
// dominate the assertion surface.
type allowAllAuth struct{}

func (allowAllAuth) Verify(_ string) error { return nil }
