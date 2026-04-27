//go:build duckdb_arrow

package migrate_test

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hugr-lab/hugen/pkg/store/local/migrate"
)

func TestEnsure_Fresh(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, migrate.Ensure(migrate.Config{
		Path:          filepath.Join(dir, "memory.db"),
		VectorSize:    384,
		EmbedderModel: "gemma-embedding",
		Seed: &migrate.SeedData{
			AgentType: migrate.SeedAgentType{ID: "hugr-data", Name: "X", Config: map[string]any{}},
			Agent:     migrate.SeedAgent{ID: "agt_ag01", ShortID: "ag01", Name: "x"},
		},
	}))
}

func TestEnsure_DimMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	require.NoError(t, migrate.Ensure(migrate.Config{
		Path: path, VectorSize: 384, EmbedderModel: "gemma-embedding",
		Seed: &migrate.SeedData{
			AgentType: migrate.SeedAgentType{ID: "hugr-data", Name: "X", Config: map[string]any{}},
			Agent:     migrate.SeedAgent{ID: "agt_ag01", ShortID: "ag01", Name: "x"},
		},
	}))

	err := migrate.Ensure(migrate.Config{Path: path, VectorSize: 768, EmbedderModel: "gemma-embedding"})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "embedding dimension mismatch"),
		"expected dimension-mismatch error, got: %v", err)
}

func TestEnsure_ModelMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	require.NoError(t, migrate.Ensure(migrate.Config{
		Path: path, VectorSize: 384, EmbedderModel: "gemma-embedding",
		Seed: &migrate.SeedData{
			AgentType: migrate.SeedAgentType{ID: "hugr-data", Name: "X", Config: map[string]any{}},
			Agent:     migrate.SeedAgent{ID: "agt_ag01", ShortID: "ag01", Name: "x"},
		},
	}))

	err := migrate.Ensure(migrate.Config{Path: path, VectorSize: 384, EmbedderModel: "other-embedding"})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "embedding model mismatch"),
		"expected model-mismatch error, got: %v", err)
}

func TestEnsure_MatchingConfigOK(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	cfg := migrate.Config{
		Path: path, VectorSize: 384, EmbedderModel: "gemma-embedding",
		Seed: &migrate.SeedData{
			AgentType: migrate.SeedAgentType{ID: "hugr-data", Name: "X", Config: map[string]any{}},
			Agent:     migrate.SeedAgent{ID: "agt_ag01", ShortID: "ag01", Name: "x"},
		},
	}
	require.NoError(t, migrate.Ensure(cfg))
	// Second run with the same config is a no-op (schema up to date).
	cfg.Seed = nil
	require.NoError(t, migrate.Ensure(cfg))
}

// TestEnsure_v002_AdditiveColumns verifies the spec-006 (schema 0.0.2)
// additive columns + indices land correctly on a fresh provision.
func TestEnsure_v002_AdditiveColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	require.NoError(t, migrate.Ensure(migrate.Config{
		Path:          path,
		VectorSize:    384,
		EmbedderModel: "gemma-embedding",
		Seed: &migrate.SeedData{
			AgentType: migrate.SeedAgentType{ID: "hugr-data", Name: "X", Config: map[string]any{}},
			Agent:     migrate.SeedAgent{ID: "agt_ag01", ShortID: "ag01", Name: "x"},
		},
	}))

	conn, err := sql.Open("duckdb", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	cases := []struct {
		table  string
		column string
	}{
		{"sessions", "session_type"},
		{"sessions", "spawned_from_event_id"},
		{"session_notes", "author_session_id"},
		{"session_events", "embedding"},
	}
	for _, c := range cases {
		t.Run(c.table+"."+c.column, func(t *testing.T) {
			var n int
			err := conn.QueryRow(
				`SELECT count(*) FROM information_schema.columns
                 WHERE table_name = ? AND column_name = ?`,
				c.table, c.column,
			).Scan(&n)
			require.NoError(t, err)
			assert.Equalf(t, 1, n, "expected column %s.%s to exist", c.table, c.column)
		})
	}

	// session_type default = 'root'
	var defaultExpr sql.NullString
	require.NoError(t, conn.QueryRow(
		`SELECT column_default FROM information_schema.columns
         WHERE table_name = 'sessions' AND column_name = 'session_type'`,
	).Scan(&defaultExpr))
	require.True(t, defaultExpr.Valid)
	assert.Contains(t, strings.ToLower(defaultExpr.String), "root")

	// Project convention: DuckDB has NO secondary indices (writes-heavy
	// workload + scale doesn't justify the maintenance cost). Indices land
	// only on Postgres deployments. We assert the absence here so a future
	// edit that accidentally drops the {{ if isPostgres }} guard fails
	// loudly on the CI DuckDB pass.
	for _, name := range []string{"idx_sessions_type", "idx_notes_author", "session_events_vss"} {
		t.Run("no-idx/"+name, func(t *testing.T) {
			var n int
			err := conn.QueryRow(
				`SELECT count(*) FROM duckdb_indexes WHERE index_name = ?`, name,
			).Scan(&n)
			require.NoError(t, err)
			assert.Equalf(t, 0, n, "DuckDB should not provision the %s index", name)
		})
	}

	// Schema version bumped.
	var ver string
	require.NoError(t, conn.QueryRow(
		`SELECT version FROM version WHERE name = 'schema'`,
	).Scan(&ver))
	assert.Equal(t, "0.0.5", ver)

	// spec 008 / migration 0.0.3 — artifacts + artifact_grants tables
	// land additively. Both must exist on a fresh DuckDB provision.
	for _, table := range []string{"artifacts", "artifact_grants"} {
		t.Run("artifacts/"+table, func(t *testing.T) {
			var n int
			err := conn.QueryRow(
				`SELECT count(*) FROM information_schema.tables WHERE table_name = ?`,
				table,
			).Scan(&n)
			require.NoError(t, err)
			assert.Equalf(t, 1, n, "expected table %s to exist", table)
		})
	}

	// Project rule: indexes only on Postgres. DuckDB stays index-free
	// for the artifact tables — verify zero secondary indexes after
	// the 0.0.3 migration.
	var artIdxCount int
	require.NoError(t, conn.QueryRow(
		`SELECT count(*) FROM duckdb_indexes
         WHERE table_name IN ('artifacts','artifact_grants')`,
	).Scan(&artIdxCount))
	assert.Equal(t, 0, artIdxCount, "DuckDB must have zero indexes on artifacts / artifact_grants")

	// spec 009 / migration 0.0.5 — approvals + tool_policies tables
	// land additively. Both must exist on a fresh DuckDB provision.
	for _, table := range []string{"approvals", "tool_policies"} {
		t.Run("phase4/"+table, func(t *testing.T) {
			var n int
			err := conn.QueryRow(
				`SELECT count(*) FROM information_schema.tables WHERE table_name = ?`,
				table,
			).Scan(&n)
			require.NoError(t, err)
			assert.Equalf(t, 1, n, "expected table %s to exist", table)
		})
	}

	// Project rule: indexes only on Postgres. DuckDB stays index-free
	// for the phase-4 tables too. Asserted here so a future edit
	// dropping `{{ if isPostgres }}` fails loudly on the CI DuckDB pass.
	var phase4IdxCount int
	require.NoError(t, conn.QueryRow(
		`SELECT count(*) FROM duckdb_indexes
         WHERE table_name IN ('approvals','tool_policies')`,
	).Scan(&phase4IdxCount))
	assert.Equal(t, 0, phase4IdxCount, "DuckDB must have zero indexes on approvals / tool_policies")
}

// TestEnsure_v002_NoVectorColumnWhenDisabled — when VectorSize == 0
// the migration must skip the embedding column / HNSW index but still
// add the discriminator columns.
func TestEnsure_v002_NoVectorColumnWhenDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	require.NoError(t, migrate.Ensure(migrate.Config{
		Path:       path,
		VectorSize: 0, // embeddings disabled
		Seed: &migrate.SeedData{
			AgentType: migrate.SeedAgentType{ID: "hugr-data", Name: "X", Config: map[string]any{}},
			Agent:     migrate.SeedAgent{ID: "agt_ag01", ShortID: "ag01", Name: "x"},
		},
	}))

	conn, err := sql.Open("duckdb", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	// Discriminator + author columns still land.
	var n int
	require.NoError(t, conn.QueryRow(
		`SELECT count(*) FROM information_schema.columns
         WHERE table_name = 'sessions' AND column_name = 'session_type'`,
	).Scan(&n))
	assert.Equal(t, 1, n)

	// embedding column should NOT exist.
	require.NoError(t, conn.QueryRow(
		`SELECT count(*) FROM information_schema.columns
         WHERE table_name = 'session_events' AND column_name = 'embedding'`,
	).Scan(&n))
	assert.Equal(t, 0, n, "embedding column should not exist when VectorSize=0")
}
