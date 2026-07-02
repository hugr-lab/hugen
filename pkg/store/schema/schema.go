// Package schema is the single source of truth for the agent store's Hugr
// schema: the GraphQL SDL, the physical DDL, and the version-to-version
// migrations. It is deliberately dependency-light so it can be imported by
//
//   - pkg/store/local/migrate — local-mode provisioning against a direct
//     DuckDB/Postgres driver connection (migrate.Ensure);
//   - the hub application (../hub) — remote-mode provisioning through the Hugr
//     app framework's per-data-source InitDBSchemaTemplate /
//     MigrateDBSchemaTemplate hooks.
//
// Both consumers render the SAME embedded templates through
// db.ParseSQLScriptTemplate; only the driver differs. hugen owns Version, so
// the agent store evolves on its own cadence independently of the hub
// platform schema (design 008 §1A / D11).
//
// The store is exposed as a standalone Hugr data source whose GraphQL path is
// `hub.db.agent` (see pkg/store/local/source.go SourceName). The SDL carries
// no @module directive — the source name provides the `.agent` nesting.
package schema

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/hugr-lab/query-engine/pkg/db"
)

// Version is the schema version this package targets. Bumped whenever a new
// migrations/<version>/ directory is added. Owned by hugen (D11).
const Version = "0.0.8"

//go:embed schema.tmpl.graphql
var sdlTmpl string

//go:embed schema.tmpl.sql
var initDDLTmpl string

//go:embed seed.tmpl.sql
var seedTmpl string

//go:embed all:migrations
var migrationsFS embed.FS

// Params are the template variables shared by the SDL and DDL renderers.
// EmbeddingsEnabled is derived (VectorSize > 0 && EmbedderModel != "").
type Params struct {
	// VectorSize is the embedding dimension. 0 disables vector search.
	VectorSize int
	// EmbedderModel is the embedding data source name referenced by the
	// @embeddings directive. Only the SDL uses it; the DDL ignores it.
	EmbedderModel string
	// IsTimescale toggles TimescaleDB hypertable support. Postgres only.
	IsTimescale bool
}

// sdlParams is the concrete template context for schema.tmpl.graphql.
type sdlParams struct {
	VectorSize        int
	EmbeddingsEnabled bool
	EmbedderModel     string
	IsTimescale       bool
}

// ddlParams is the concrete template context for schema.tmpl.sql and the
// migration scripts.
type ddlParams struct {
	VectorSize  int
	IsTimescale bool
}

// SDL renders the GraphQL SDL for the given (attached) DB type. dbType is
// typically db.SDBAttachedDuckDB or db.SDBAttachedPostgres.
func SDL(dbType db.ScriptDBType, p Params) (string, error) {
	out, err := db.ParseSQLScriptTemplate(dbType, sdlTmpl, sdlParams{
		VectorSize:        p.VectorSize,
		EmbeddingsEnabled: p.VectorSize > 0 && p.EmbedderModel != "",
		EmbedderModel:     p.EmbedderModel,
		IsTimescale:       p.IsTimescale,
	})
	if err != nil {
		return "", fmt.Errorf("schema: render sdl: %w", err)
	}
	return out, nil
}

// InitDDL renders the full physical schema for a fresh database. dbType is
// typically db.SDBDuckDB or db.SDBPostgres (direct driver), or the attached
// variants when rendering for the Hugr app framework.
func InitDDL(dbType db.ScriptDBType, p Params) (string, error) {
	out, err := db.ParseSQLScriptTemplate(dbType, initDDLTmpl, ddlParams{
		VectorSize:  p.VectorSize,
		IsTimescale: p.IsTimescale,
	})
	if err != nil {
		return "", fmt.Errorf("schema: render init ddl: %w", err)
	}
	return out, nil
}

// MigrateDDL renders the concatenated migration SQL that upgrades a database
// from `from` (exclusive) to Version (inclusive), in version+filename order.
// Returns an empty string when `from` already equals Version. This is exactly
// the blob the Hugr app framework's MigrateDBSchemaTemplate(fromVersion) wants,
// and what migrate.upgrade applies in local mode.
func MigrateDDL(dbType db.ScriptDBType, from string, p Params) (string, error) {
	scripts, err := Migrations(from, Version)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, s := range scripts {
		body, err := migrationsFS.ReadFile(s.Path)
		if err != nil {
			return "", fmt.Errorf("schema: read %s: %w", s.Path, err)
		}
		rendered, err := db.ParseSQLScriptTemplate(dbType, string(body), ddlParams{
			VectorSize:  p.VectorSize,
			IsTimescale: p.IsTimescale,
		})
		if err != nil {
			return "", fmt.Errorf("schema: render %s: %w", s.Path, err)
		}
		b.WriteString(rendered)
		if !strings.HasSuffix(rendered, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String(), nil
}

// SeedData is written to agent_types + agents on first-run provisioning.
type SeedData struct {
	AgentType SeedAgentType
	Agent     SeedAgent
}

// SeedAgentType is the initial agent_types row.
type SeedAgentType struct {
	ID          string
	Name        string
	Description string
	Config      any // marshalled to JSON at render time
}

// SeedAgent is the initial agents row.
type SeedAgent struct {
	ID      string
	ShortID string
	Name    string
}

// SeedSQL renders the seed insert for the given DB type.
func SeedSQL(dbType db.ScriptDBType, seed SeedData) (string, error) {
	data, err := seedParamsFrom(seed)
	if err != nil {
		return "", err
	}
	out, err := db.ParseSQLScriptTemplate(dbType, seedTmpl, data)
	if err != nil {
		return "", fmt.Errorf("schema: render seed: %w", err)
	}
	return out, nil
}

// seedParams is the rendered template context for seed.tmpl.sql. Config is
// SQL-escaped JSON ready to inline between single quotes.
type seedParams struct {
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

func seedParamsFrom(in SeedData) (seedParams, error) {
	var out seedParams
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
			return out, fmt.Errorf("schema: marshal seed config: %w", err)
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

// Script identifies one migration file under migrations/<version>/.
type Script struct {
	Path     string
	Version  string
	Filename string
}

// Migrations returns the embedded migration scripts whose version is > from
// and <= to, sorted by (version, filename). Layout mirrors hugr cmd/migrate:
// migrations/<version>/<N-name>.sql.
func Migrations(from, to string) ([]Script, error) {
	var scripts []Script
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
		if CompareVersions(ver, from) <= 0 || CompareVersions(ver, to) > 0 {
			return nil
		}
		scripts = append(scripts, Script{Path: p, Version: ver, Filename: path.Base(p)})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("schema: walk migrations: %w", err)
	}
	slices.SortFunc(scripts, func(a, b Script) int {
		if c := CompareVersions(a.Version, b.Version); c != 0 {
			return c
		}
		return strings.Compare(a.Filename, b.Filename)
	})
	return scripts, nil
}

// CompareVersions compares dot-separated version strings numerically.
func CompareVersions(a, b string) int {
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
