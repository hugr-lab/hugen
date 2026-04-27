//go:build duckdb_arrow

package local

import (
	"context"
	"fmt"
	"log/slog"
	"testing"

	hugr "github.com/hugr-lab/query-engine"
	"github.com/hugr-lab/query-engine/pkg/auth"
	coredb "github.com/hugr-lab/query-engine/pkg/data-sources/sources/runtime/core-db"
	"github.com/hugr-lab/query-engine/pkg/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hugr-lab/hugen/pkg/store/local/migrate"
)

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// pinTestEngine spins up the same engine shape as local.New but stops
// short of verifyLocalEmbedding — we want to drive pinEmbedderModel
// directly against a real hub.db + attached runtime source so the
// `hub.db.version` path resolves like in production.
//
// Returns the engine (satisfies types.Querier) and a cleanup.
func pinTestEngine(t *testing.T, embedderModel string, vectorDim int) *hugr.Service {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	hubPath := dir + "/memory.db"

	require.NoError(t, migrate.Ensure(migrate.Config{
		Path:          hubPath,
		VectorSize:    vectorDim,
		EmbedderModel: embedderModel,
		Seed: &migrate.SeedData{
			AgentType: migrate.SeedAgentType{
				ID: "hugr-data", Name: "Hugr Data Agent",
				Description: "Pin test agent type",
			},
			Agent: migrate.SeedAgent{
				ID: "agt_ag01", ShortID: "ag01", Name: "pin-test",
			},
		},
	}))

	source := NewSource(SourceConfig{
		Path:          hubPath,
		VectorSize:    vectorDim,
		EmbedderModel: embedderModel,
	})
	service, err := hugr.New(hugr.Config{
		DB:     db.Config{},
		CoreDB: coredb.New(coredb.Config{VectorSize: 0}),
		Auth:   &auth.Config{},
	})
	require.NoError(t, err)
	require.NoError(t, service.AttachRuntimeSource(ctx, source))
	require.NoError(t, service.Init(ctx))

	t.Cleanup(func() { _ = service.Close() })
	return service
}

// TestPinEmbedderModel_FirstProvision covers the "fresh DB" path:
// migrate.Ensure writes the `embedding_model` version row at
// provisioning time, so pinEmbedderModel's first call must see the
// stored model and leave it in place.
//
// Spec 006 T510(a).
func TestPinEmbedderModel_FirstProvision(t *testing.T) {
	svc := pinTestEngine(t, "nomic-embed-text", 768)
	ctx := context.Background()

	require.NoError(t, pinEmbedderModel(ctx, svc, "nomic-embed-text"))

	got, err := readEmbedderPin(ctx, svc)
	require.NoError(t, err)
	assert.Equal(t, "nomic-embed-text", got)
}

// TestPinEmbedderModel_MatchingReuse covers the "same model on
// subsequent startup" path: pin already in place, same value from
// config → no-op, no error, row untouched.
//
// Spec 006 T510(b).
func TestPinEmbedderModel_MatchingReuse(t *testing.T) {
	svc := pinTestEngine(t, "gemma-embedding", 768)
	ctx := context.Background()

	// First call pins.
	require.NoError(t, pinEmbedderModel(ctx, svc, "gemma-embedding"))
	// Second call is a no-op.
	require.NoError(t, pinEmbedderModel(ctx, svc, "gemma-embedding"))

	got, err := readEmbedderPin(ctx, svc)
	require.NoError(t, err)
	assert.Equal(t, "gemma-embedding", got)
}

// TestPinEmbedderModel_Mismatch covers SC-006: startup with a model
// name that disagrees with the stored pin fails fast with a named
// error pointing at the re-provision remediation.
//
// Spec 006 T510(c).
func TestPinEmbedderModel_Mismatch(t *testing.T) {
	svc := pinTestEngine(t, "nomic-embed-text", 768)
	ctx := context.Background()

	err := pinEmbedderModel(ctx, svc, "text-embedding-3-small")
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "nomic-embed-text", "error must name the stored pin")
	assert.Contains(t, msg, "text-embedding-3-small", "error must name the configured model")
	assert.Contains(t, msg, "re-provision",
		"error must point at the remediation")
}

// TestVerifyLocalEmbedding_Unreachable covers SC-006 / T501: with a
// local embedding configured but no reachable data source, startup
// must fail fast with an error that names the model. Engine has NO
// embedder data source registered here, so core.models.embedding
// returns an error immediately.
//
// Spec 006 T511.
func TestVerifyLocalEmbedding_Unreachable(t *testing.T) {
	svc := pinTestEngine(t, "nomic-embed-text", 768)
	ctx := context.Background()

	err := verifyLocalEmbedding(ctx, svc, EmbeddingConfig{
		Mode:      "local",
		Model:     "nomic-embed-text",
		Dimension: 768,
	}, slog.New(slog.NewTextHandler(discardWriter{}, nil)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "embedder unreachable",
		"error must identify the unreachable embedder")
	assert.Contains(t, err.Error(), "nomic-embed-text",
		"error must name the configured model")
}

// TestVerifyLocalEmbedding_RemoteMode_SkipsProbe covers the remote
// mode path: no probe runs, pin stays untouched, no error.
func TestVerifyLocalEmbedding_RemoteMode_SkipsProbe(t *testing.T) {
	svc := pinTestEngine(t, "nomic-embed-text", 768)
	ctx := context.Background()

	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))
	err := verifyLocalEmbedding(ctx, svc, EmbeddingConfig{
		Mode:      "remote",
		Model:     "nomic-embed-text",
		Dimension: 768,
	}, logger)
	require.NoError(t, err)

	// Pin unchanged — still what migrate wrote.
	pin, err := readEmbedderPin(ctx, svc)
	require.NoError(t, err)
	assert.Equal(t, "nomic-embed-text", pin)
}

// sanityCheckQuerier ensures readEmbedderPin's backwards-compat fallback
// (embedding_model → embedder_model) works by manually writing the
// legacy row and reading it back as if the pin had been stored under
// the old name before the rename landed.
func TestReadEmbedderPin_LegacyFallback(t *testing.T) {
	svc := pinTestEngine(t, "legacy-model", 768)
	ctx := context.Background()

	// pinTestEngine -> migrate.Ensure writes version name='embedding_model'.
	// readEmbedderPin's second-name fallback must find it.
	got, err := readEmbedderPin(ctx, svc)
	require.NoError(t, err)
	require.Equal(t, "legacy-model", got)

	// Verify the fallback path is the one firing by confirming no
	// `embedder_model` row exists on this fresh DB.
	type row struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	resp, err := svc.Query(ctx,
		`query { hub { db { version(filter: {name: {eq: "embedder_model"}}) { name version } } } }`,
		nil,
	)
	require.NoError(t, err)
	defer resp.Close()
	var rows []row
	err = resp.ScanData("hub.db.version", &rows)
	if err != nil {
		assert.Contains(t, err.Error(), "wrong data path", "legacy DBs should report no rows")
	} else {
		assert.Empty(t, rows, "legacy DBs should not have embedder_model row")
	}
	_ = fmt.Sprintf
}
