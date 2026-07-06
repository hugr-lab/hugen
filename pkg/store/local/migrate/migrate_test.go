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
	"github.com/hugr-lab/hugen/pkg/store/schema"
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

// TestEnsure_FreshSchema verifies the full pruned table set + additive
// columns + index discipline land correctly on a fresh provision. The
// schema is squashed to a single 0.0.8 baseline (pre-v1), so there is
// no upgrade-through-scripts path to exercise — schema.tmpl.sql is the
// only source of truth.
func TestEnsure_FreshSchema(t *testing.T) {
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

	// Schema version stamped at the package's current Version.
	var ver string
	require.NoError(t, conn.QueryRow(
		`SELECT version FROM version WHERE name = 'schema'`,
	).Scan(&ver))
	assert.Equal(t, schema.Version, ver)

	// Pruned tables (Phase-7 memory + dormant artifacts/approvals set)
	// must NOT be provisioned — the schema is trimmed to hugen's working
	// set. Asserted so a future edit re-adding them fails loudly.
	for _, table := range []string{
		"memory_items", "memory_log", "memory_tags", "memory_links",
		"hypotheses", "session_reviews", "session_participants",
		"approvals", "artifacts", "artifact_grants",
	} {
		t.Run("pruned/"+table, func(t *testing.T) {
			var n int
			err := conn.QueryRow(
				`SELECT count(*) FROM information_schema.tables WHERE table_name = ?`,
				table,
			).Scan(&n)
			require.NoError(t, err)
			assert.Equalf(t, 0, n, "pruned table %s must not exist", table)
		})
	}

	// tool_policies (spec 009 / phase 4) is KEPT and lands on a fresh
	// provision, index-free on DuckDB.
	var toolPolN int
	require.NoError(t, conn.QueryRow(
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'tool_policies'`,
	).Scan(&toolPolN))
	assert.Equal(t, 1, toolPolN, "expected tool_policies table to exist")

	var phase4IdxCount int
	require.NoError(t, conn.QueryRow(
		`SELECT count(*) FROM duckdb_indexes WHERE table_name = 'tool_policies'`,
	).Scan(&phase4IdxCount))
	assert.Equal(t, 0, phase4IdxCount, "DuckDB must have zero indexes on tool_policies")

	// Dynamic-skills tables (Phase 6.2.db) are KEPT and land on a fresh
	// provision.
	for _, table := range []string{"skills", "skill_log", "skill_links"} {
		t.Run("skills/"+table, func(t *testing.T) {
			var n int
			err := conn.QueryRow(
				`SELECT count(*) FROM information_schema.tables WHERE table_name = ?`,
				table,
			).Scan(&n)
			require.NoError(t, err)
			assert.Equalf(t, 1, n, "expected table %s to exist", table)
		})
	}

	// Phase 4.2.3 / migration 0.0.6 — session_notes gets four new
	// columns (category, author_role, mission, embedding). Verified
	// here on a fresh provision so the schema template stays in sync
	// with the ALTER-based migration.
	for _, col := range []string{"category", "author_role", "mission"} {
		t.Run("notepad-cols/"+col, func(t *testing.T) {
			var n int
			err := conn.QueryRow(
				`SELECT count(*) FROM information_schema.columns
                 WHERE table_name = 'session_notes' AND column_name = ?`,
				col,
			).Scan(&n)
			require.NoError(t, err)
			assert.Equalf(t, 1, n, "expected session_notes.%s after 0.0.6", col)
		})
	}
	// Embedding column lands only when VectorSize > 0; this run uses
	// the default seed config (which has VectorSize = 768 in
	// mustFreshConn). Mirrors the session_events check below.
	var notepadEmbed int
	require.NoError(t, conn.QueryRow(
		`SELECT count(*) FROM information_schema.columns
         WHERE table_name = 'session_notes' AND column_name = 'embedding'`,
	).Scan(&notepadEmbed))
	assert.Equal(t, 1, notepadEmbed, "session_notes.embedding should exist when VectorSize > 0")

	// Phase 6 / migration 0.0.7 — tasks + task_log tables land
	// additively. Both must exist on a fresh DuckDB provision.
	for _, table := range []string{"tasks", "task_log"} {
		t.Run("phase6/"+table, func(t *testing.T) {
			var n int
			err := conn.QueryRow(
				`SELECT count(*) FROM information_schema.tables WHERE table_name = ?`,
				table,
			).Scan(&n)
			require.NoError(t, err)
			assert.Equalf(t, 1, n, "expected table %s to exist", table)
		})
	}
	// Project rule: indexes only on Postgres. DuckDB must have zero
	// secondary indexes on tasks / task_log.
	var phase6IdxCount int
	require.NoError(t, conn.QueryRow(
		`SELECT count(*) FROM duckdb_indexes
         WHERE table_name IN ('tasks','task_log')`,
	).Scan(&phase6IdxCount))
	assert.Equal(t, 0, phase6IdxCount, "DuckDB must have zero indexes on tasks / task_log")
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

	// Phase 4.2.3 — session_notes.embedding must also be gated by
	// VectorSize. The 0.0.6 ALTER block sits inside a
	// {{ if gt .VectorSize 0 }} guard; without it, the migration
	// would attempt to add a vector column to a non-vector build.
	require.NoError(t, conn.QueryRow(
		`SELECT count(*) FROM information_schema.columns
         WHERE table_name = 'session_notes' AND column_name = 'embedding'`,
	).Scan(&n))
	assert.Equal(t, 0, n, "session_notes.embedding should not exist when VectorSize=0")

	// But the three text columns DO exist (gate is on embedding only).
	for _, col := range []string{"category", "author_role", "mission"} {
		require.NoError(t, conn.QueryRow(
			`SELECT count(*) FROM information_schema.columns
             WHERE table_name = 'session_notes' AND column_name = ?`,
			col,
		).Scan(&n))
		assert.Equalf(t, 1, n, "session_notes.%s should exist regardless of VectorSize", col)
	}
}
