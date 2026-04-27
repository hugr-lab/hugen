// Package migrate provisions and upgrades the agent memory database.
//
// Runs on a DIRECT driver connection (duckdb-go or pgx), not through the hugr
// query-engine. This keeps DDL statements unqualified and lets us support
// Postgres' native CREATE DATABASE semantics that aren't expressible through
// the engine's attached-catalog view.
//
// Layout (embedded):
//
//	schema.tmpl.sql         — initial schema, applied when the DB is created
//	seed.tmpl.sql           — optional initial rows (agent_type + agent)
//	migrations/<version>/   — upgrade scripts, sorted by numeric filename prefix
//
// Call Ensure(ctx, Config) once at agent startup before the engine is
// initialised. Idempotent: subsequent calls are no-ops when the schema is
// already at the target version.
package migrate

import (
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"

	_ "github.com/duckdb/duckdb-go/v2" // register "duckdb" driver
	"github.com/hugr-lab/query-engine/pkg/data-sources/sources"
	"github.com/hugr-lab/query-engine/pkg/db"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // register "pgx" driver
)

// SchemaVersion is the version that Ensure targets.
const SchemaVersion = "0.0.5"

//go:embed schema.tmpl.sql
var initSchemaTmpl string

//go:embed seed.tmpl.sql
var seedTmpl string

//go:embed all:migrations
var migrationsFS embed.FS

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

// SeedData is written to agent_types + agents on first-run provisioning.
type SeedData struct {
	AgentType SeedAgentType
	Agent     SeedAgent
}

type SeedAgentType struct {
	ID          string
	Name        string
	Description string
	Config      any // marshalled to JSON at render time
}

type SeedAgent struct {
	ID      string
	ShortID string
	Name    string
}

// Ensure provisions or migrates the memory DB at cfg.Path.
//
// First run:
//  1. open direct connection (creating the file if needed for DuckDB; CREATE
//     DATABASE for Postgres)
//  2. run schema.tmpl.sql
//  3. write version row
//  4. run seed.tmpl.sql when cfg.Seed is non-nil
//
// Subsequent runs: walk migrations/ and apply scripts up to TargetVersion.
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

// ── first-run provisioning ─────────────────────────────────────

func provision(dbType db.ScriptDBType, cfg Config, target string) error {
	conn, err := openForCreate(dbType, cfg.Path)
	if err != nil {
		return fmt.Errorf("migrate: open for create: %w", err)
	}
	defer func() { _ = conn.Close() }()

	rendered, err := db.ParseSQLScriptTemplate(dbType, initSchemaTmpl, SchemaParams{
		VectorSize:  cfg.VectorSize,
		IsTimescale: cfg.IsTimescale,
	})
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
		data, err := seedData(cfg.Seed)
		if err != nil {
			return err
		}
		rendered, err := db.ParseSQLScriptTemplate(dbType, seedTmpl, data)
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

	if compareVersions(version, target) == 0 {
		return nil
	}
	if compareVersions(version, target) > 0 {
		return fmt.Errorf("migrate: db version %s is newer than target %s", version, target)
	}

	scripts, err := collectMigrations(version, target)
	if err != nil {
		return err
	}
	if len(scripts) == 0 {
		return fmt.Errorf("migrate: no migration scripts from %s to %s", version, target)
	}

	for _, script := range scripts {
		body, err := migrationsFS.ReadFile(script.path)
		if err != nil {
			return fmt.Errorf("migrate: read %s: %w", script.path, err)
		}
		rendered, err := db.ParseSQLScriptTemplate(dbType, string(body), SchemaParams{
			VectorSize:  cfg.VectorSize,
			IsTimescale: cfg.IsTimescale,
		})
		if err != nil {
			return fmt.Errorf("migrate: render %s: %w", script.path, err)
		}
		if _, err := conn.Exec(rendered); err != nil {
			return fmt.Errorf("migrate: apply %s: %w", script.path, err)
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
// stored at provision time. A mismatch is fatal: existing vectors in
// memory_items are not re-computable and the agent must be recreated.
// Rows may be missing for DBs provisioned before embedding version tracking —
// in that case we write them from the current config (one-time backfill).
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

// ── migration discovery ─────────────────────────────────────────

type migrationScript struct {
	path     string
	version  string
	filename string
}

// collectMigrations walks the embedded migrations/ FS and returns scripts
// whose folder version > `from` and <= `to`, sorted by (version, filename).
// Uses the same layout as hugr cmd/migrate: migrations/<version>/<N-name>.sql
func collectMigrations(from, to string) ([]migrationScript, error) {
	var scripts []migrationScript

	err := fs.WalkDir(migrationsFS, "migrations", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".sql") {
			return nil
		}
		rel := strings.TrimPrefix(p, "migrations"+string(os.PathSeparator))
		rel = strings.TrimPrefix(rel, "migrations/")
		parts := strings.SplitN(rel, "/", 2)
		if len(parts) < 2 {
			return nil // scripts must live under a version directory
		}
		ver := parts[0]
		if compareVersions(ver, from) <= 0 || compareVersions(ver, to) > 0 {
			return nil
		}
		scripts = append(scripts, migrationScript{
			path:     p,
			version:  ver,
			filename: path.Base(p),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("migrate: walk migrations: %w", err)
	}

	slices.SortFunc(scripts, func(a, b migrationScript) int {
		if c := compareVersions(a.version, b.version); c != 0 {
			return c
		}
		return strings.Compare(a.filename, b.filename)
	})
	return scripts, nil
}

// compareVersions compares dot-separated version strings numerically.
// Adapted from hugr cmd/migrate/main.go.
func compareVersions(a, b string) int {
	ap := strings.Split(a, ".")
	bp := strings.Split(b, ".")
	n := len(ap)
	if len(bp) > n {
		n = len(bp)
	}
	for i := 0; i < n; i++ {
		var ai, bi int
		if i < len(ap) {
			ai, _ = strconv.Atoi(ap[i])
		}
		if i < len(bp) {
			bi, _ = strconv.Atoi(bp[i])
		}
		if ai < bi {
			return -1
		}
		if ai > bi {
			return 1
		}
	}
	return 0
}

// ── template params ─────────────────────────────────────────────

// SchemaParams is passed to schema.tmpl.sql and migration templates.
type SchemaParams struct {
	VectorSize  int
	IsTimescale bool
}

// SeedParams is the rendered template context for seed.tmpl.sql. Config is
// SQL-escaped JSON ready to inline between single quotes.
type SeedParams struct {
	AgentType seedAgentType
	Agent     seedAgent
}

type seedAgentType struct {
	ID          string
	Name        string
	Description string
	Config      string
}

type seedAgent struct {
	ID      string
	ShortID string
	Name    string
}

func seedData(in *SeedData) (SeedParams, error) {
	var out SeedParams
	out.AgentType.ID = escapeSQL(in.AgentType.ID)
	out.AgentType.Name = escapeSQL(in.AgentType.Name)
	out.AgentType.Description = escapeSQL(in.AgentType.Description)

	switch v := in.AgentType.Config.(type) {
	case nil:
		out.AgentType.Config = "{}"
	case string:
		out.AgentType.Config = escapeSQL(v)
	case []byte:
		out.AgentType.Config = escapeSQL(string(v))
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return out, fmt.Errorf("migrate: marshal seed config: %w", err)
		}
		out.AgentType.Config = escapeSQL(string(b))
	}

	out.Agent.ID = escapeSQL(in.Agent.ID)
	out.Agent.ShortID = escapeSQL(in.Agent.ShortID)
	out.Agent.Name = escapeSQL(in.Agent.Name)
	return out, nil
}

func escapeSQL(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// ── misc ────────────────────────────────────────────────────────

func scriptTypeForPath(path string) db.ScriptDBType {
	if strings.HasPrefix(path, "postgres://") {
		return db.SDBPostgres
	}
	return db.SDBDuckDB
}
