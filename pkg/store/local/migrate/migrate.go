// Package migrate provisions and upgrades the agent store database.
//
// Runs on a DIRECT driver connection (duckdb-go or pgx), not through the hugr
// query-engine. This keeps DDL statements unqualified and lets us support
// Postgres' native CREATE DATABASE semantics that aren't expressible through
// the engine's attached-catalog view.
//
// The schema itself (DDL, seed, migrations) lives in
// github.com/hugr-lab/hugen/pkg/store/schema — the single source of truth
// shared with the (future) hub provisioning path. This package owns only the
// PROVISIONING logic: connection management, the version-table bookkeeping,
// and the embedder-pin verification.
//
// Call Ensure(ctx, Config) once at agent startup before the engine is
// initialised. Idempotent: subsequent calls are no-ops when the schema is
// already at the target version.
package migrate

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	_ "github.com/duckdb/duckdb-go/v2" // register "duckdb" driver
	"github.com/hugr-lab/query-engine/pkg/data-sources/sources"
	"github.com/hugr-lab/query-engine/pkg/db"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // register "pgx" driver

	"github.com/hugr-lab/hugen/pkg/store/schema"
)

// SchemaVersion is the version that Ensure targets. Owned by pkg/store/schema.
const SchemaVersion = schema.Version

// SeedData / SeedAgentType / SeedAgent are re-exported from pkg/store/schema
// so existing callers keep referencing migrate.SeedData.
type (
	SeedData      = schema.SeedData
	SeedAgentType = schema.SeedAgentType
	SeedAgent     = schema.SeedAgent
)

// Config controls Ensure.
type Config struct {
	// Path is the DuckDB file path or `postgres://...` DSN.
	Path string

	// VectorSize is the embedding dimension. 0 disables vector search.
	// Frozen at first-run provision — mismatches on later runs are fatal.
	VectorSize int

	// EmbedderModel is the embedding data source name. Stored alongside
	// VectorSize at provision time and verified on subsequent runs.
	// Empty means "vector search disabled".
	EmbedderModel string

	// IsTimescale toggles TimescaleDB hypertable creation. Postgres only.
	IsTimescale bool

	// Seed is the optional initial agent_type + agent. When nil, first-run
	// provision creates only the schema.
	Seed *SeedData

	// TargetVersion overrides SchemaVersion. Empty means "use SchemaVersion".
	// Useful for tests that need to pin to a specific version.
	TargetVersion string
}

// Ensure provisions or migrates the store DB at cfg.Path.
//
// First run:
//  1. open direct connection (creating the file if needed for DuckDB; CREATE
//     DATABASE for Postgres)
//  2. apply schema.InitDDL
//  3. write version row
//  4. apply schema.SeedSQL when cfg.Seed is non-nil
//
// Subsequent runs: apply schema.MigrateDDL up to TargetVersion, then bump the
// version row.
func Ensure(cfg Config) error {
	if cfg.Path == "" {
		return errors.New("migrate: Path required")
	}
	target := cfg.TargetVersion
	if target == "" {
		target = SchemaVersion
	}

	dbType := scriptTypeForPath(cfg.Path)

	exists, err := dbExists(dbType, cfg.Path)
	if err != nil {
		return fmt.Errorf("migrate: check db exists: %w", err)
	}

	if !exists {
		return provision(dbType, cfg, target)
	}

	return upgrade(dbType, cfg, target)
}

// schemaParams builds the schema render context from cfg.
func schemaParams(cfg Config) schema.Params {
	return schema.Params{
		VectorSize:    cfg.VectorSize,
		EmbedderModel: cfg.EmbedderModel,
		IsTimescale:   cfg.IsTimescale,
	}
}

// ── first-run provisioning ─────────────────────────────────────

func provision(dbType db.ScriptDBType, cfg Config, target string) error {
	conn, err := openForCreate(dbType, cfg.Path)
	if err != nil {
		return fmt.Errorf("migrate: open for create: %w", err)
	}
	defer func() { _ = conn.Close() }()

	rendered, err := schema.InitDDL(dbType, schemaParams(cfg))
	if err != nil {
		return fmt.Errorf("migrate: render schema: %w", err)
	}
	if _, err := conn.Exec(rendered); err != nil {
		return fmt.Errorf("migrate: apply schema: %w", err)
	}

	if _, err := conn.Exec(
		`INSERT INTO version (name, version) VALUES ('schema', $1)`,
		target,
	); err != nil {
		return fmt.Errorf("migrate: write version: %w", err)
	}

	if _, err := conn.Exec(
		`INSERT INTO version (name, version) VALUES ('embedding_model', $1), ('embedding_dim', $2)`,
		cfg.EmbedderModel, strconv.Itoa(cfg.VectorSize),
	); err != nil {
		return fmt.Errorf("migrate: write embedding version: %w", err)
	}

	if cfg.Seed != nil {
		rendered, err := schema.SeedSQL(dbType, *cfg.Seed)
		if err != nil {
			return fmt.Errorf("migrate: render seed: %w", err)
		}
		if _, err := conn.Exec(rendered); err != nil {
			return fmt.Errorf("migrate: apply seed: %w", err)
		}
	}
	return nil
}

// ── upgrade path ────────────────────────────────────────────────

func upgrade(dbType db.ScriptDBType, cfg Config, target string) error {
	conn, err := openExisting(dbType, cfg.Path)
	if err != nil {
		return fmt.Errorf("migrate: open: %w", err)
	}
	defer func() { _ = conn.Close() }()

	var version string
	if err := conn.QueryRow(
		`SELECT version FROM version WHERE name = 'schema' LIMIT 1`,
	).Scan(&version); err != nil {
		return fmt.Errorf("migrate: read version: %w", err)
	}

	if err := verifyEmbedding(conn, cfg); err != nil {
		return err
	}

	if schema.CompareVersions(version, target) == 0 {
		return nil
	}
	if schema.CompareVersions(version, target) > 0 {
		return fmt.Errorf("migrate: db version %s is newer than target %s", version, target)
	}

	blob, err := schema.MigrateDDL(dbType, version, schemaParams(cfg))
	if err != nil {
		return err
	}
	if strings.TrimSpace(blob) != "" {
		if _, err := conn.Exec(blob); err != nil {
			return fmt.Errorf("migrate: apply migrations %s -> %s: %w", version, target, err)
		}
	}

	if _, err := conn.Exec(
		`UPDATE version SET version = $1 WHERE name = 'schema'`, target,
	); err != nil {
		return fmt.Errorf("migrate: update version: %w", err)
	}
	return nil
}

// verifyEmbedding compares the configured embedding model+dim to what was
// stored at provision time. A mismatch is fatal: existing vectors are not
// re-computable and the agent must be recreated. Rows may be missing for DBs
// provisioned before embedding version tracking — in that case we write them
// from the current config (one-time backfill).
func verifyEmbedding(conn *sql.DB, cfg Config) error {
	var storedModel, storedDim sql.NullString
	_ = conn.QueryRow(
		`SELECT version FROM version WHERE name = 'embedding_model' LIMIT 1`,
	).Scan(&storedModel)
	_ = conn.QueryRow(
		`SELECT version FROM version WHERE name = 'embedding_dim' LIMIT 1`,
	).Scan(&storedDim)

	cfgDim := strconv.Itoa(cfg.VectorSize)

	if !storedModel.Valid && !storedDim.Valid {
		if _, err := conn.Exec(
			`INSERT INTO version (name, version) VALUES ('embedding_model', $1), ('embedding_dim', $2)`,
			cfg.EmbedderModel, cfgDim,
		); err != nil {
			return fmt.Errorf("migrate: backfill embedding version: %w", err)
		}
		return nil
	}

	if storedModel.String != cfg.EmbedderModel {
		return fmt.Errorf(
			"migrate: embedding model mismatch — DB was provisioned with %q, config has %q. Delete %s and re-create the agent to change models",
			storedModel.String, cfg.EmbedderModel, cfg.Path,
		)
	}
	if storedDim.String != cfgDim {
		return fmt.Errorf(
			"migrate: embedding dimension mismatch — DB was provisioned with dim=%s, config has dim=%s. Delete %s and re-create the agent to change dimensions",
			storedDim.String, cfgDim, cfg.Path,
		)
	}
	return nil
}

// ── connection helpers ──────────────────────────────────────────

func dbExists(dbType db.ScriptDBType, path string) (bool, error) {
	switch dbType {
	case db.SDBDuckDB:
		if strings.HasPrefix(path, "s3://") {
			return false, errors.New("migrate: s3 duckdb paths not supported")
		}
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return false, nil
		} else if err != nil {
			return false, err
		}
		c, err := sql.Open("duckdb", path)
		if err != nil {
			return false, err
		}
		defer func() { _ = c.Close() }()
		return true, c.Ping()
	case db.SDBPostgres:
		c, err := sql.Open("pgx", path)
		if err != nil {
			return false, err
		}
		defer func() { _ = c.Close() }()
		err = c.Ping()
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "3D000" {
			return false, nil // invalid_catalog_name → database does not exist
		}
		if err != nil {
			return false, err
		}
		return true, nil
	default:
		return false, fmt.Errorf("migrate: unsupported db type %q", dbType)
	}
}

func openForCreate(dbType db.ScriptDBType, path string) (*sql.DB, error) {
	switch dbType {
	case db.SDBDuckDB:
		return sql.Open("duckdb", path)
	case db.SDBPostgres:
		dsn, err := sources.ParseDSN(path)
		if err != nil {
			return nil, err
		}
		targetDB := dsn.DBName
		dsn.DBName = "postgres"
		bootstrap, err := sql.Open("pgx", dsn.String())
		if err != nil {
			return nil, err
		}
		if _, err := bootstrap.Exec(`CREATE DATABASE "` + targetDB + `"`); err != nil {
			_ = bootstrap.Close()
			return nil, fmt.Errorf("create database %s: %w", targetDB, err)
		}
		_ = bootstrap.Close()
		return sql.Open("pgx", path)
	default:
		return nil, fmt.Errorf("migrate: unsupported db type %q", dbType)
	}
}

func openExisting(dbType db.ScriptDBType, path string) (*sql.DB, error) {
	switch dbType {
	case db.SDBDuckDB:
		return sql.Open("duckdb", path)
	case db.SDBPostgres:
		return sql.Open("pgx", path)
	default:
		return nil, fmt.Errorf("migrate: unsupported db type %q", dbType)
	}
}

// ── misc ────────────────────────────────────────────────────────

func scriptTypeForPath(path string) db.ScriptDBType {
	if strings.HasPrefix(path, "postgres://") {
		return db.SDBPostgres
	}
	return db.SDBDuckDB
}
