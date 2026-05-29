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

// TestEnsure_v007_UpgradePath provisions a DB pinned at 0.0.6
// (the prior schema version) then re-runs Ensure to upgrade it to
// 0.0.7. Verifies that the migration script lands the new tasks +
// task_log tables on an existing deployment — not just on a fresh
// provision. Critical for production where users' DBs were created
// before the Phase 6 schema landed.
func TestEnsure_v007_UpgradePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	baseCfg := migrate.Config{
		Path: path, VectorSize: 384, EmbedderModel: "gemma-embedding",
		Seed: &migrate.SeedData{
			AgentType: migrate.SeedAgentType{ID: "hugr-data", Name: "X", Config: map[string]any{}},
			Agent:     migrate.SeedAgent{ID: "agt_ag01", ShortID: "ag01", Name: "x"},
		},
	}

	// Step 1: provision at 0.0.6.
	pin := baseCfg
	pin.TargetVersion = "0.0.6"
	require.NoError(t, migrate.Ensure(pin))

	conn, err := sql.Open("duckdb", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	// schema.tmpl.sql always ships the latest table set, so a
	// provision pinned to 0.0.6 still creates tasks + task_log
	// here — only the version row tracks "0.0.6". To exercise the
	// real upgrade path we DROP the 0.0.7 tables so the migration
	// script has to recreate them on the bump below. CREATE TABLE
	// IF NOT EXISTS in the migration script keeps it idempotent
	// for users on already-upgraded DBs.
	for _, table := range []string{"task_log", "tasks"} {
		_, err := conn.Exec(`DROP TABLE IF EXISTS ` + table)
		require.NoError(t, err)
	}
	for _, table := range []string{"tasks", "task_log"} {
		var n int
		require.NoError(t, conn.QueryRow(
			`SELECT count(*) FROM information_schema.tables WHERE table_name = ?`, table,
		).Scan(&n))
		require.Equalf(t, 0, n, "drop should remove %s", table)
	}

	// Step 2: upgrade to 0.0.7 (pinned — the default target has since
	// advanced past 0.0.7).
	bump := baseCfg
	bump.Seed = nil // seed already present
	bump.TargetVersion = "0.0.7"
	require.NoError(t, migrate.Ensure(bump))

	// Verify version row + tables.
	var ver string
	require.NoError(t, conn.QueryRow(
		`SELECT version FROM version WHERE name = 'schema'`,
	).Scan(&ver))
	assert.Equal(t, "0.0.7", ver)

	for _, table := range []string{"tasks", "task_log"} {
		var n int
		require.NoError(t, conn.QueryRow(
			`SELECT count(*) FROM information_schema.tables WHERE table_name = ?`, table,
		).Scan(&n))
		assert.Equalf(t, 1, n, "table %s must exist after 0.0.7 upgrade", table)
	}
}

// TestEnsure_v008_UpgradePath provisions a DB pinned at 0.0.7 then
// upgrades it to 0.0.8, verifying that the migration script lands the
// dynamic-skills tables (skills + skill_log + skill_links) on an
// existing deployment. Mirrors the v007 upgrade-path test: drop the
// tables that schema.tmpl.sql ships unconditionally, then let the
// migration recreate them on the version bump.
func TestEnsure_v008_UpgradePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	baseCfg := migrate.Config{
		Path: path, VectorSize: 384, EmbedderModel: "gemma-embedding",
		Seed: &migrate.SeedData{
			AgentType: migrate.SeedAgentType{ID: "hugr-data", Name: "X", Config: map[string]any{}},
			Agent:     migrate.SeedAgent{ID: "agt_ag01", ShortID: "ag01", Name: "x"},
		},
	}

	// Step 1: provision at 0.0.7.
	pin := baseCfg
	pin.TargetVersion = "0.0.7"
	require.NoError(t, migrate.Ensure(pin))

	conn, err := sql.Open("duckdb", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	// Drop the 0.0.8 tables so the migration must recreate them.
	for _, table := range []string{"skill_links", "skill_log", "skills"} {
		_, err := conn.Exec(`DROP TABLE IF EXISTS ` + table)
		require.NoError(t, err)
	}

	// Step 2: upgrade to 0.0.8 (default target).
	bump := baseCfg
	bump.Seed = nil
	require.NoError(t, migrate.Ensure(bump))

	var ver string
	require.NoError(t, conn.QueryRow(
		`SELECT version FROM version WHERE name = 'schema'`,
	).Scan(&ver))
	assert.Equal(t, "0.0.8", ver)

	for _, table := range []string{"skills", "skill_log", "skill_links"} {
		var n int
		require.NoError(t, conn.QueryRow(
			`SELECT count(*) FROM information_schema.tables WHERE table_name = ?`, table,
		).Scan(&n))
		assert.Equalf(t, 1, n, "table %s must exist after 0.0.8 upgrade", table)
	}

	// Spot-check a representative column from each table.
	for _, c := range []struct{ table, col string }{
		{"skills", "description_vec"},
		{"skills", "metadata"},
		{"skills", "source"},
		{"skill_log", "event"},
		{"skill_links", "relation"},
	} {
		var n int
		require.NoError(t, conn.QueryRow(
			`SELECT count(*) FROM information_schema.columns WHERE table_name = ? AND column_name = ?`,
			c.table, c.col,
		).Scan(&n))
		assert.Equalf(t, 1, n, "expected %s.%s after 0.0.8 upgrade", c.table, c.col)
	}
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
	assert.Equal(t, "0.0.8", ver)

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
