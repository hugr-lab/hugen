//go:build duckdb_arrow && integration

// Package http integration tests wire the http adapter to a REAL
// runtime stack — DuckDB-backed pkg/store/local, real
// session.Manager, real handlers. The unit-level tests in
// this package cover handler shapes via fakeHost; this file covers
// the persistence-and-state-agreement half of SC-002 / SC-010
// ("API list and DB direct inspection always agree").
//
// Build tag: `integration` — hidden from `go test ./...`. Run with:
//
//	go test -tags='duckdb_arrow integration' ./pkg/adapter/http/...
package http

import (
	"context"
	"encoding/json"
	"log/slog"
	stdhttp "net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	hugr "github.com/hugr-lab/query-engine"
	"github.com/hugr-lab/query-engine/pkg/auth"
	coredb "github.com/hugr-lab/query-engine/pkg/data-sources/sources/runtime/core-db"
	"github.com/hugr-lab/query-engine/pkg/db"
	"github.com/stretchr/testify/require"

	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/store/local"
	"github.com/hugr-lab/hugen/pkg/store/local/migrate"
	"github.com/hugr-lab/hugen/pkg/store/queries"
)

const (
	intgAgentID = "agt-int01"
	intgAgentNm = "hugen-integration"
)

// realStoreEngine spins up a fresh DuckDB hub.db under t.TempDir
// and returns the *hugr.Service (satisfies types.Querier). Cleanup
// is registered automatically.
func realStoreEngine(t *testing.T) *hugr.Service {
	t.Helper()
	hubPath := filepath.Join(t.TempDir(), "hub.db")
	require.NoError(t, migrate.Ensure(migrate.Config{
		Path:          hubPath,
		VectorSize:    0,
		EmbedderModel: "",
		Seed: &migrate.SeedData{
			AgentType: migrate.SeedAgentType{
				ID:          "hugen-integration",
				Name:        "Integration Agent",
				Description: "agent type for http integration tests",
			},
			Agent: migrate.SeedAgent{
				ID:      intgAgentID,
				ShortID: "int01",
				Name:    intgAgentNm,
			},
		},
	}))

	source := local.NewSource(local.SourceConfig{
		Path:          hubPath,
		VectorSize:    0,
		EmbedderModel: "",
	})
	service, err := hugr.New(hugr.Config{
		DB:     db.Config{},
		CoreDB: coredb.New(coredb.Config{VectorSize: 0}),
		Auth:   &auth.Config{},
	})
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, service.AttachRuntimeSource(ctx, source))
	require.NoError(t, service.Init(ctx))
	t.Cleanup(func() { _ = service.Close() })
	return service
}

// stubIdentitySource fills session.NewAgent's required dep without
// reaching out to a remote IdP.
type stubIdentitySource struct{}

func (stubIdentitySource) Agent(_ context.Context) (identity.Agent, error) {
	return identity.Agent{ID: intgAgentID, Name: intgAgentNm}, nil
}
func (stubIdentitySource) WhoAmI(_ context.Context) (identity.WhoAmI, error) {
	return identity.WhoAmI{UserID: intgAgentID}, nil
}
func (stubIdentitySource) Permission(_ context.Context, _, _ string) (identity.Permission, error) {
	return identity.Permission{Enabled: true}, nil
}

// nullModel satisfies model.Model so NewModelRouter accepts the
// default-intent registration. The integration lifecycle test
// doesn't post a user_message that would trigger inference, so
// Generate is unreachable.
type nullModel struct{}

func (nullModel) Spec() model.ModelSpec {
	return model.ModelSpec{Provider: "stub", Name: "noop"}
}
func (nullModel) Generate(_ context.Context, _ model.Request) (model.Stream, error) {
	panic("nullModel.Generate must not be called from the lifecycle test")
}

// TestIntegration_LifecyclePersistsThroughLocalStore opens, lists,
// and closes a session via the HTTP API and asserts each step is
// reflected in hub.db on disk via direct GraphQL query.
//
// Scope: Open / List / Close — the persistence-and-state-agreement
// half of SC-002 and SC-010. Streaming and POST are NOT exercised
// here: integrationHost.Subscribe returns a fresh local channel
// (not the runtime's fan-out) and integrationHost.Submit is wired
// to ErrSessionClosed. A future test that posts user_messages and
// observes the real session pump's replay/live ordering still
// needs to be written; this one covers SC-002's
// API-vs-DB-agreement half.
//
// Skipped from `go test ./...` by the integration build tag.
func TestIntegration_LifecyclePersistsThroughLocalStore(t *testing.T) {
	engine := realStoreEngine(t)
	store := session.NewRuntimeStoreLocal(engine, false)

	agent, err := session.NewAgent(intgAgentID, intgAgentNm, stubIdentitySource{})
	require.NoError(t, err)

	// Minimal model router: one stub model under the default intent.
	router, err := model.NewModelRouter(
		map[model.Intent]model.ModelSpec{
			model.IntentDefault: {Provider: "stub", Name: "noop"},
		},
		map[model.ModelSpec]model.Model{
			{Provider: "stub", Name: "noop"}: nullModel{},
		},
	)
	require.NoError(t, err)

	mgr := session.NewManager(store, agent, router,
		session.NewCommandRegistry(), protocol.NewCodec(), nil)
	rt := session.NewRuntime(mgr, nil, nil)

	mux := stdhttp.NewServeMux()
	a, err := NewAdapter(Options{
		Mux:    mux,
		Auth:   allowAllAuth{},
		Codec:  protocol.NewCodec(),
		Replay: store,
	})
	require.NoError(t, err)

	runCtx, runCancel := context.WithCancel(context.Background())
	t.Cleanup(runCancel)

	// We need an AdapterHost to feed Run. Build one by hand: we
	// don't want the runtime to manage it (we own runCancel here).
	hostDone := make(chan struct{})
	go func() {
		defer close(hostDone)
		_ = a.Run(runCtx, runtimeAdapterHost(rt))
	}()
	<-a.Mounted()
	a.MarkReady()
	t.Cleanup(func() {
		runCancel()
		<-hostDone
		_ = rt.Shutdown(context.Background())
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// 1. Open a session via the API; assert hub.db has the row.
	openResp := doJSON(t, srv, "POST", "/api/v1/sessions", "tok",
		map[string]any{"metadata": map[string]any{"label": "integration"}})
	var open OpenSessionResponse
	require.NoError(t, json.NewDecoder(openResp.Body).Decode(&open))
	openResp.Body.Close()
	require.NotEmpty(t, open.SessionID)

	rows, err := queries.RunQuery[[]map[string]any](context.Background(), engine,
		`query ($id: String!) {
			hub { db { agent {
				sessions(filter: {id: {eq: $id}}, limit: 1) {
					id status agent_id metadata
				}
			}}}
		}`,
		map[string]any{"id": open.SessionID},
		"hub.db.agent.sessions",
	)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "active", rows[0]["status"])
	require.Equal(t, intgAgentID, rows[0]["agent_id"])

	// 2. List sessions via the API; assert it includes our row
	// with the same metadata it persisted.
	listResp := doJSON(t, srv, "GET", "/api/v1/sessions?status=active", "tok", nil)
	var list ListSessionsResponse
	require.NoError(t, json.NewDecoder(listResp.Body).Decode(&list))
	listResp.Body.Close()
	found := false
	for _, s := range list.Sessions {
		if s.SessionID == open.SessionID {
			found = true
			require.Equal(t, "active", s.Status)
			if s.Metadata["label"] != "integration" {
				t.Errorf("metadata roundtrip lost: %v", s.Metadata)
			}
		}
	}
	require.True(t, found, "API list missed session %s", open.SessionID)

	// 3. Close the session via the API; assert hub.db now reports
	// closed status.
	closeResp := doJSON(t, srv, "POST", "/api/v1/sessions/"+open.SessionID+"/close", "tok",
		map[string]any{"reason": "test_done"})
	var closed CloseSessionResponse
	require.NoError(t, json.NewDecoder(closeResp.Body).Decode(&closed))
	closeResp.Body.Close()
	require.Equal(t, "closed", closed.Status)

	// manager.Close is fully synchronous (s.emit + UpdateSessionStatus
	// both block before the handler writes its response), so by the
	// time closeResp returned the row's status is already "closed".
	// Verify directly.
	verifyRows, err := queries.RunQuery[[]map[string]any](context.Background(), engine,
		`query ($id: String!) {
			hub { db { agent {
				sessions(filter: {id: {eq: $id}}, limit: 1) {
					id status
				}
			}}}
		}`,
		map[string]any{"id": open.SessionID},
		"hub.db.agent.sessions",
	)
	require.NoError(t, err)
	require.Len(t, verifyRows, 1)
	require.Equal(t, "closed", verifyRows[0]["status"])

	// 4. List with status=closed via the API; assert agreement
	// with the direct query (SC-010).
	listClosedResp := doJSON(t, srv, "GET", "/api/v1/sessions?status=closed", "tok", nil)
	var listClosed ListSessionsResponse
	require.NoError(t, json.NewDecoder(listClosedResp.Body).Decode(&listClosed))
	listClosedResp.Body.Close()
	apiClosed := false
	for _, s := range listClosed.Sessions {
		if s.SessionID == open.SessionID {
			apiClosed = true
		}
	}
	require.True(t, apiClosed, "API list status=closed missed session %s", open.SessionID)
}

// runtimeAdapterHost converts a *session.Runtime into an
// AdapterHost. The runtime exposes its host implementation only via
// Start; we need it directly for the integration test that drives
// the http adapter's Run loop without the runtime supervisor.
//
// The runtime package has an internal adapterHost type that does
// exactly this; we get to it by calling Start on a separate
// goroutine that we never let block — but that's complex. Simpler:
// build a thin wrapper that satisfies AdapterHost using the
// runtime's Manager directly.
type integrationHost struct {
	rt *session.Runtime
}

func runtimeAdapterHost(rt *session.Runtime) session.AdapterHost {
	return &integrationHost{rt: rt}
}

func (h *integrationHost) OpenSession(ctx context.Context, req session.OpenRequest) (*session.Session, time.Time, error) {
	s, openedAt, err := h.rt.Manager().Open(ctx, req)
	if err != nil {
		return nil, time.Time{}, err
	}
	return s, openedAt, nil
}

func (h *integrationHost) ResumeSession(ctx context.Context, id string) (*session.Session, error) {
	return h.rt.Manager().Resume(ctx, id)
}

func (h *integrationHost) Submit(_ context.Context, _ protocol.Frame) error {
	// The lifecycle test doesn't exercise post; calling Submit
	// would spin the session goroutine and reach into the
	// (nil) model. Return ErrSessionClosed so the post path is a
	// loud no-op instead of a panic — same code the API reports.
	return session.ErrSessionClosed
}

func (h *integrationHost) Subscribe(ctx context.Context, sessionID string) (<-chan protocol.Frame, error) {
	c := make(chan protocol.Frame, 16)
	go func() {
		<-ctx.Done()
		close(c)
	}()
	_ = sessionID
	return c, nil
}

func (h *integrationHost) CloseSession(ctx context.Context, id, reason string) (time.Time, error) {
	return h.rt.Manager().Close(ctx, id, reason)
}

func (h *integrationHost) ListSessions(ctx context.Context, status string) ([]session.SessionSummary, error) {
	return h.rt.Manager().List(ctx, status)
}

func (h *integrationHost) Logger() *slog.Logger { return slog.Default() }
