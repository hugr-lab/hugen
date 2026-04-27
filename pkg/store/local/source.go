// Package local brings up the embedded hugr engine for hub.db: runs the
// driver-level migrations, constructs the engine, attaches hub.db as a
// RuntimeSource, registers LLM/embedding data sources, and probes the
// embedding dimension.
//
// Only imported in local-DB mode. In remote-hub mode cmd/agent uses the
// hugr client directly and pkg/store/local is never linked.
//
// This file declares the RuntimeSource that ATTACH-es the provisioned
// memory DB into the engine's pool and renders the GraphQL SDL. The DB
// file itself is provisioned by pkg/store/local/migrate before the
// engine starts.
package local

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	"github.com/hugr-lab/query-engine/pkg/catalog/compiler"
	cs "github.com/hugr-lab/query-engine/pkg/catalog/sources"
	"github.com/hugr-lab/query-engine/pkg/data-sources/sources"
	"github.com/hugr-lab/query-engine/pkg/db"
	"github.com/hugr-lab/query-engine/pkg/engines"
	"github.com/hugr-lab/query-engine/types"
)

// SourceName is the attached DB name and GraphQL path prefix.
// Produces the GraphQL path `{ hub { db { ... } } }`.
const SourceName = "hub.db"

//go:embed schema.tmpl.graphql
var schemaGraphQLTmpl string

// SourceConfig configures the hub.db RuntimeSource.
type SourceConfig struct {
	// Path to the provisioned memory DB (must already exist — use
	// migrate.Ensure before calling Attach).
	// If prefixed with "postgres://", the source is treated as Postgres.
	Path string

	// ReadOnly attaches the DB as READ_ONLY.
	ReadOnly bool

	// VectorSize is the embedding dimension. 0 disables vector search.
	VectorSize int

	// EmbedderModel is the name of the embedding data source registered in
	// the engine. Referenced by the @embeddings directive on memory_items.
	EmbedderModel string

	// IsTimescale toggles TimescaleDB hypertable support in SDL. Postgres only.
	IsTimescale bool
}

// Source implements sources.RuntimeSource.
type Source struct {
	cfg    SourceConfig
	dbType types.DataSourceType
	engine engines.Engine
}

// NewSource returns a RuntimeSource for the given memory DB.
func NewSource(cfg SourceConfig) *Source {
	if strings.HasPrefix(cfg.Path, "postgres://") {
		return &Source{
			cfg:    cfg,
			dbType: sources.Postgres,
			engine: engines.NewPostgres(),
		}
	}
	return &Source{
		cfg:    cfg,
		dbType: sources.DuckDB,
		engine: engines.NewDuckDB(),
	}
}

func (s *Source) Name() string                         { return SourceName }
func (s *Source) Engine() engines.Engine               { return s.engine }
func (s *Source) IsReadonly() bool                     { return s.cfg.ReadOnly }
func (s *Source) AsModule() bool                       { return true }
func (s *Source) DataSourceType() types.DataSourceType { return s.dbType }

// Attach issues ATTACH DATABASE against the engine's pool. The DB must
// already contain the schema — run migrate.Ensure first.
func (s *Source) Attach(ctx context.Context, pool *db.Pool) error {
	if err := sources.CheckDBExists(ctx, pool, s.Name(), s.dbType); err != nil {
		return err
	}

	path := s.cfg.Path
	if path == "" {
		path = ":memory:"
	}

	stmt := "ATTACH DATABASE '" + path + "' AS \"" + s.Name() + "\""
	switch {
	case s.dbType == sources.DuckDB && s.IsReadonly():
		stmt += " (READ_ONLY)"
	case s.dbType == sources.Postgres && !s.IsReadonly():
		stmt += " (TYPE POSTGRES)"
	case s.dbType == sources.Postgres && s.IsReadonly():
		stmt += " (TYPE POSTGRES, READ_ONLY)"
	}

	if _, err := pool.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("hubdb: attach %s: %w", path, err)
	}
	return nil
}

// Catalog renders the SDL template and returns the source for schema compilation.
func (s *Source) Catalog(ctx context.Context) (cs.Catalog, error) {
	opts := compiler.Options{
		Name:         s.Name(),
		Prefix:       graphQLPrefix(s.Name()),
		AsModule:     s.AsModule(),
		ReadOnly:     s.IsReadonly(),
		EngineType:   string(s.engine.Type()),
		Capabilities: s.engine.Capabilities(),
	}

	dbType := db.SDBAttachedDuckDB
	if s.dbType == sources.Postgres {
		dbType = db.SDBAttachedPostgres
	}

	rendered, err := db.ParseSQLScriptTemplate(dbType, schemaGraphQLTmpl, SDLParams{
		VectorSize:        s.cfg.VectorSize,
		EmbeddingsEnabled: s.cfg.VectorSize > 0 && s.cfg.EmbedderModel != "",
		EmbedderModel:     s.cfg.EmbedderModel,
		IsTimescale:       s.cfg.IsTimescale,
	})
	if err != nil {
		return nil, fmt.Errorf("hubdb: render sdl: %w", err)
	}

	return cs.NewStringSource(s.Name(), s.engine, opts, rendered)
}

// SDLParams are the template variables used by schema.tmpl.graphql.
type SDLParams struct {
	VectorSize        int
	EmbeddingsEnabled bool
	EmbedderModel     string
	IsTimescale       bool
}

// graphQLPrefix maps a dotted catalog name (e.g. "hub.db") to a valid
// GraphQL identifier by replacing "." with "_". Dots are illegal in
// GraphQL type names and break variable declarations like
// `$data: hub.db_agents_mut_input_data!`.
func graphQLPrefix(name string) string {
	return strings.ReplaceAll(name, ".", "_")
}
