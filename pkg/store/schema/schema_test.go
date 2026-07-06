package schema

import (
	"strings"
	"testing"

	"github.com/hugr-lab/query-engine/pkg/db"
)

// keptTables is the pruned working set the SDL + DDL must define.
var keptTables = []string{
	"version", "agent_types", "agents",
	"sessions", "session_events", "session_notes",
	"skills", "skill_log", "skill_links",
	"tasks", "task_log", "tool_policies",
}

// removedTables must NOT appear anywhere in the schema after the prune.
var removedTables = []string{
	"memory_items", "memory_log", "memory_tags", "memory_links",
	"hypotheses", "session_reviews", "session_participants",
	"approvals", "artifacts", "artifact_grants", "session_artifacts",
	"session_events_full", "session_events_chain", "session_notes_chain",
}

func TestVersion_NonEmpty(t *testing.T) {
	if Version == "" {
		t.Fatal("Version must be non-empty")
	}
}

func TestSDL_ContainsKeptTables_NoModule_NoRemoved(t *testing.T) {
	sdl, err := SDL(db.SDBAttachedDuckDB, Params{VectorSize: 0})
	if err != nil {
		t.Fatalf("SDL: %v", err)
	}
	if strings.TrimSpace(sdl) == "" {
		t.Fatal("SDL rendered empty")
	}
	for _, tbl := range keptTables {
		if !strings.Contains(sdl, `@table(name: "`+tbl+`"`) {
			t.Errorf("SDL missing kept table %q", tbl)
		}
	}
	for _, tbl := range removedTables {
		// @table / @view definitions for removed tables must be gone. (Bare
		// prose mentions are fine, so we key on the directive form.)
		if strings.Contains(sdl, `@table(name: "`+tbl+`"`) ||
			strings.Contains(sdl, `@view(`+"\n"+`  name: "`+tbl+`"`) {
			t.Errorf("SDL still defines removed table/view %q", tbl)
		}
	}
	// The store is a standalone source at hub.agent.db — no per-type @module.
	for _, line := range strings.Split(sdl, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue // comment lines may mention @module in prose
		}
		if strings.Contains(line, "@module") {
			t.Errorf("SDL carries a live @module directive: %q", strings.TrimSpace(line))
		}
	}
}

func TestSDL_EmbeddingsToggle(t *testing.T) {
	off, err := SDL(db.SDBAttachedDuckDB, Params{VectorSize: 0})
	if err != nil {
		t.Fatalf("SDL off: %v", err)
	}
	on, err := SDL(db.SDBAttachedDuckDB, Params{VectorSize: 384, EmbedderName: "embedder"})
	if err != nil {
		t.Fatalf("SDL on: %v", err)
	}
	// Key on the directive form `@embeddings(` — docstrings mention the word
	// "@embeddings" in prose regardless of the toggle.
	if strings.Contains(off, "@embeddings(") {
		t.Error("VectorSize=0 must not emit an @embeddings directive")
	}
	if !strings.Contains(on, "@embeddings(") {
		t.Error("VectorSize>0 with an embedder must emit an @embeddings directive")
	}
}

func TestInitDDL_CreatesKept_NotRemoved(t *testing.T) {
	ddl, err := InitDDL(db.SDBDuckDB, Params{VectorSize: 0})
	if err != nil {
		t.Fatalf("InitDDL: %v", err)
	}
	for _, tbl := range keptTables {
		if !strings.Contains(ddl, "CREATE TABLE IF NOT EXISTS "+tbl) {
			t.Errorf("InitDDL missing CREATE TABLE for kept %q", tbl)
		}
	}
	for _, tbl := range removedTables {
		if strings.Contains(ddl, "CREATE TABLE IF NOT EXISTS "+tbl+" ") ||
			strings.Contains(ddl, "CREATE TABLE IF NOT EXISTS "+tbl+"\n") {
			t.Errorf("InitDDL still creates removed %q", tbl)
		}
	}
}

func TestInitDDL_Postgres_Renders(t *testing.T) {
	ddl, err := InitDDL(db.SDBPostgres, Params{VectorSize: 384})
	if err != nil {
		t.Fatalf("InitDDL postgres: %v", err)
	}
	if !strings.Contains(ddl, "CREATE TABLE IF NOT EXISTS sessions") {
		t.Error("postgres InitDDL missing sessions")
	}
}

func TestMigrateDDL_Stream(t *testing.T) {
	// The pre-v1 squash left 0.0.8 as the baseline; the stream restarts at
	// 0.0.9 (agents.role). Upgrading from the baseline applies it…
	out, err := MigrateDDL(db.SDBDuckDB, "0.0.8", Params{})
	if err != nil {
		t.Fatalf("MigrateDDL: %v", err)
	}
	if !strings.Contains(out, "ALTER TABLE agents ADD COLUMN IF NOT EXISTS role") {
		t.Errorf("MigrateDDL from 0.0.8 missing agents.role migration, got %q", out)
	}
	// …and a database already at Version gets nothing.
	out, err = MigrateDDL(db.SDBDuckDB, Version, Params{})
	if err != nil {
		t.Fatalf("MigrateDDL at Version: %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("MigrateDDL at Version should be empty, got %q", out)
	}
}

func TestSeedSQL_RendersConfig(t *testing.T) {
	sql, err := SeedSQL(db.SDBDuckDB, SeedData{
		AgentType: SeedAgentType{ID: "t1", Name: "type", Config: map[string]any{"k": "v"}},
		Agent:     SeedAgent{ID: "a1", ShortID: "a1", Name: "agent"},
	})
	if err != nil {
		t.Fatalf("SeedSQL: %v", err)
	}
	if !strings.Contains(sql, "t1") || !strings.Contains(sql, "a1") {
		t.Errorf("SeedSQL missing seeded ids: %q", sql)
	}
}

func TestSeedSQL_EscapesQuotes(t *testing.T) {
	// A single quote in a value must be doubled so the inlined SQL stays valid.
	sql, err := SeedSQL(db.SDBDuckDB, SeedData{
		AgentType: SeedAgentType{ID: "t", Name: "O'Brien"},
		Agent:     SeedAgent{ID: "a", ShortID: "a", Name: "n"},
	})
	if err != nil {
		t.Fatalf("SeedSQL: %v", err)
	}
	if !strings.Contains(sql, "O''Brien") {
		t.Errorf("single quote not escaped: %q", sql)
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.0.8", "0.0.8", 0},
		{"0.0.7", "0.0.8", -1},
		{"0.0.9", "0.0.8", 1},
		{"0.1.0", "0.0.9", 1},
		{"1.0.0", "0.9.9", 1},
		{"0.0.10", "0.0.9", 1}, // numeric, not lexical
		{"0.0", "0.0.0", 0},    // missing components treated as zero
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
