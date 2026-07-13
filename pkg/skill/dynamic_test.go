//go:build duckdb_arrow

package skill

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	hugr "github.com/hugr-lab/query-engine"
	"github.com/hugr-lab/query-engine/pkg/auth"
	coredb "github.com/hugr-lab/query-engine/pkg/data-sources/sources/runtime/core-db"
	"github.com/hugr-lab/query-engine/pkg/db"
	"github.com/hugr-lab/query-engine/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hugr-lab/hugen/pkg/store/local"
	"github.com/hugr-lab/hugen/pkg/store/local/migrate"
	"github.com/hugr-lab/hugen/pkg/store/queries"
)

// newDynamicTestEngine boots a fresh DuckDB-backed hub.db + hugr
// engine (embeddings DISABLED — db-1 tests exercise the keyword /
// metadata path, not live semantic search) and returns the querier
// plus the seeded agent id. Mirrors pkg/scheduler/store.newTestStore.
func newDynamicTestEngine(t *testing.T) (types.Querier, string) {
	t.Helper()
	ctx := context.Background()
	hubPath := filepath.Join(t.TempDir(), "memory.db")

	require.NoError(t, migrate.Ensure(migrate.Config{
		Path:          hubPath,
		VectorSize:    0,
		EmbedderModel: "",
		Seed: &migrate.SeedData{
			AgentType: migrate.SeedAgentType{ID: "test-agent-type", Name: "Test"},
			Agent:     migrate.SeedAgent{ID: "agt_test01", ShortID: "ts01", Name: "test"},
		},
	}))

	source := local.NewSource(local.SourceConfig{Path: hubPath, VectorSize: 0})
	svc, err := hugr.New(hugr.Config{
		DB:     db.Config{},
		CoreDB: coredb.New(coredb.Config{VectorSize: 0}),
		Auth:   &auth.Config{},
	})
	require.NoError(t, err)
	require.NoError(t, svc.AttachRuntimeSource(ctx, source))
	require.NoError(t, svc.Init(ctx))
	t.Cleanup(func() { _ = svc.Close() })
	return svc, "agt_test01"
}

const changeReportSKILL = `---
name: change-report
description: Summarise what changed in a dataset over a window.
metadata:
  hugen:
    task:
      eligible: true
      kind: worker
      goal_summary: Produce a change report.
      inputs_schema:
        type: object
        required: [dataset]
        properties:
          dataset: {type: string}
    mission:
      keywords: [change, report, diff]
    tier_compatibility: [worker]
---
This is the change-report recipe body.
`

const plainHelperSKILL = `---
name: plain-helper
description: A plain non-task helper skill.
metadata:
  hugen:
    tier_compatibility: [worker]
---
plain body
`

// dataCatalogSKILL is an ordinary category skill that admits a task tool
// via allowed-tools — the post-dissolution shape (no recipe_catalog flag,
// no special index type). Kept named "data-catalog" so the shared-state
// subtests below still reference it.
const dataCatalogSKILL = `---
name: data-catalog
description: A category skill grouping data recipes.
allowed-tools:
  - provider: task
    tools: [change-report]
metadata:
  hugen:
    requires_skills: []
    tier_compatibility: [root, mission, worker]
---
catalog body
`

func mustParse(t *testing.T, raw string) Manifest {
	t.Helper()
	m, err := Parse([]byte(raw))
	require.NoError(t, err)
	return m
}

// TestDynamicBackend exercises the full db-1 backend surface against a
// real engine: Publish (dir + DB index), List (DB, metadata-only),
// Get (disk, full body), Reconcile (dir↔DB), Uninstall, and the
// keyword-fallback search path. One engine boot, ordered subtests.
func TestDynamicBackend(t *testing.T) {
	q, agentID := newDynamicTestEngine(t)
	ctx := context.Background()
	root := t.TempDir()
	b := newDynamicBackend(root, "" /* hubRoot */, q, agentID, false /* embedderEnabled */, nil)

	t.Run("Publish writes dir + index row", func(t *testing.T) {
		m := mustParse(t, changeReportSKILL)
		require.NoError(t, b.Publish(ctx, m, nil, PublishOptions{}))

		// Bundle landed on disk.
		_, err := os.Stat(filepath.Join(root, "change-report", "SKILL.md"))
		require.NoError(t, err)

		// Index row landed with denormalised columns.
		rows, err := b.index.listAll(ctx)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		r := rows[0]
		assert.Equal(t, "change-report", r.Name)
		assert.Equal(t, "recipe", r.Type)
		assert.Equal(t, "authored", r.Source)
		assert.True(t, r.TaskEligible)
		assert.Equal(t, "worker", r.TaskKind)
		assert.True(t, r.HasInputsSchema)
		assert.Contains(t, r.Keywords, "report")
		assert.Contains(t, r.TierCompat, "worker")
		assert.NotEmpty(t, r.ContentHash)
		assert.NotEmpty(t, r.BundlePath)
	})

	t.Run("List serves manifest from DB index (no body)", func(t *testing.T) {
		skills, err := b.List(ctx)
		require.NoError(t, err)
		require.Len(t, skills, 1)
		s := skills[0]
		assert.Equal(t, OriginDynamic, s.Origin)
		assert.Equal(t, "change-report", s.Manifest.Name)
		// Typed Hugen projection reconstructed from metadata JSON.
		assert.True(t, s.Manifest.Hugen.Task.Eligible)
		assert.NotEmpty(t, s.Manifest.Hugen.Task.InputsSchema)
		assert.Contains(t, s.Manifest.Hugen.Mission.Keywords, "diff")
		// List is metadata-only — body not loaded.
		assert.Empty(t, s.Manifest.Body)
		// FS handle points at the bundle for lazy content.
		assert.NotNil(t, s.FS)
		// db-2: the DB-index id is carried onto the Skill so usage
		// logging (skill_log.skill_id FK) can reference it.
		assert.True(t, strings.HasPrefix(s.ID, "skl-"), "Skill.ID = %q", s.ID)
	})

	t.Run("LogSkillEvents appends skill_log rows (skip empty id)", func(t *testing.T) {
		rows, err := b.index.listAll(ctx)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		id := rows[0].ID

		// One real id + one empty (skipped) → exactly one row written.
		require.NoError(t, b.LogSkillEvents(ctx, []string{id, ""}, SkillLogShown, "ses-1"))
		require.NoError(t, b.LogSkillEvents(ctx, []string{id}, SkillLogUsed, "ses-1"))

		type logRow struct {
			SkillID string `json:"skill_id"`
			Event   string `json:"event"`
		}
		logs, err := queries.RunQuery[[]logRow](ctx, q,
			`query ($agent: String!) {
				hub { agent { db { skill_log(filter: {agent_id: {eq: $agent}}) { skill_id event } } } }
			}`,
			map[string]any{"agent": agentID},
			"hub.agent.db.skill_log",
		)
		require.NoError(t, err)
		// Append-only: two rows (shown + used), both for the real id; the
		// empty id contributed nothing.
		require.Len(t, logs, 2)
		events := map[string]int{}
		for _, l := range logs {
			assert.Equal(t, id, l.SkillID)
			events[l.Event]++
		}
		assert.Equal(t, 1, events[SkillLogShown])
		assert.Equal(t, 1, events[SkillLogUsed])

		// db-2: the recall projection's log_bucket_aggregation surfaces
		// the per-event tallies in ONE query (alongside the candidate),
		// and pin:{eq:false} keeps the non-pinned change-report.
		type bucketRow struct {
			ID  string `json:"id"`
			Log []struct {
				Key struct {
					Event string `json:"event"`
				} `json:"key"`
				Aggregations struct {
					RowsCount int `json:"_rows_count"`
				} `json:"aggregations"`
			} `json:"log_bucket_aggregation"`
		}
		withLog, err := queries.RunQuery[[]bucketRow](ctx, q,
			`query ($agent: String!) {
				hub { agent { db {
					skills(filter: {agent_id: {eq: $agent}, pin: {eq: false}}) {
						id log_bucket_aggregation { key { event } aggregations { _rows_count } }
					}
				}}}
			}`,
			map[string]any{"agent": agentID},
			"hub.agent.db.skills",
		)
		require.NoError(t, err)
		require.Len(t, withLog, 1)
		counts := map[string]int{}
		for _, bkt := range withLog[0].Log {
			counts[bkt.Key.Event] = bkt.Aggregations.RowsCount
		}
		assert.Equal(t, 1, counts[SkillLogShown], "shown count via log_bucket_aggregation")
		assert.Equal(t, 1, counts[SkillLogUsed], "used count via log_bucket_aggregation")
	})

	t.Run("aliased dynamic+pinned query extracts both at hub.agent.db", func(t *testing.T) {
		// Validates the 2b data layer: two aliased skills() selections in
		// one query, extracted at the "hub.agent.db" object path into a
		// struct. No semantic here (the test engine has no embedder), so
		// `dynamic` is just the non-pinned set. change-report is not
		// pinned → dynamic has it, pinned is empty.
		// The recall projection pulls everything the advertise needs —
		// id + name + description + per-event stats — in the same query,
		// for BOTH aliases. (Same recallProjection recallRanked uses.)
		resp, err := q.Query(ctx,
			`query ($agent: String!) {
				hub { agent { db {
					dynamic: skills(filter: {agent_id: {eq: $agent}, pin: {eq: false}}) {`+recallProjection+`}
					pinned:  skills(filter: {agent_id: {eq: $agent}, pin: {eq: true}}) {`+recallProjection+`}
				}}}
			}`,
			map[string]any{"agent": agentID},
		)
		require.NoError(t, err)
		defer resp.Close()
		require.NoError(t, resp.Err())

		var dyn []rankedSkillRow
		require.NoError(t, resp.ScanDataJSON("hub.agent.db.dynamic", &dyn))
		require.Len(t, dyn, 1, "non-pinned change-report in dynamic alias")
		assert.Equal(t, "change-report", dyn[0].Name)
		counts := map[string]int{}
		for _, bkt := range dyn[0].Log {
			counts[bkt.Key.Event] = bkt.Aggregations.RowsCount
		}
		assert.Equal(t, 1, counts[SkillLogShown], "shown via aliased recall")
		assert.Equal(t, 1, counts[SkillLogUsed], "used via aliased recall")

		// pinned alias is empty here (no pinned skills) — ScanData may
		// report no-data; either way it yields zero rows.
		var pin []rankedSkillRow
		_ = resp.ScanData("hub.agent.db.pinned", &pin)
		assert.Empty(t, pin, "no pinned skills installed in this test")
	})

	t.Run("Get reads full bundle from disk (with body)", func(t *testing.T) {
		s, err := b.Get(ctx, "change-report")
		require.NoError(t, err)
		assert.Equal(t, OriginDynamic, s.Origin)
		assert.Contains(t, string(s.Manifest.Body), "change-report recipe body")
	})

	t.Run("Publish update keeps the same id", func(t *testing.T) {
		before, err := b.index.getRowByName(ctx, "authored", "change-report")
		require.NoError(t, err)
		require.NotEmpty(t, before.ID)

		// Re-publish with overwrite — same name/source → update in place.
		m := mustParse(t, changeReportSKILL)
		require.NoError(t, b.Publish(ctx, m, nil, PublishOptions{Overwrite: true}))

		after, err := b.index.getRowByName(ctx, "authored", "change-report")
		require.NoError(t, err)
		assert.Equal(t, before.ID, after.ID, "id must survive an update")

		rows, err := b.index.listAll(ctx)
		require.NoError(t, err)
		assert.Len(t, rows, 1, "update must not create a duplicate row")
	})

	t.Run("Reconcile indexes a bundle dropped directly on disk", func(t *testing.T) {
		// Write a second bundle straight to disk (operator drop —
		// bypasses Publish, so no index row yet).
		dir := filepath.Join(root, "plain-helper")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(plainHelperSKILL), 0o644))

		n, err := b.Reconcile(ctx)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, n, 2, "reconcile should index both bundles")

		rows, err := b.index.listAll(ctx)
		require.NoError(t, err)
		assert.Len(t, rows, 2)

		ph, err := b.index.getRowByName(ctx, "authored", "plain-helper")
		require.NoError(t, err)
		assert.Equal(t, "skill", ph.Type)
		assert.False(t, ph.TaskEligible)
	})

	t.Run("category skill indexes as an ordinary skill", func(t *testing.T) {
		// A category skill that admits a task tool via allowed-tools is an
		// ordinary skill post-dissolution — type "skill", no special index
		// treatment. Published here so the shared-state Uninstall subtest
		// below still sees it in the index.
		cat := mustParse(t, dataCatalogSKILL)
		require.NoError(t, b.Publish(ctx, cat, nil, PublishOptions{}))

		_, err := b.Reconcile(ctx)
		require.NoError(t, err)

		row, err := b.index.getRowByName(ctx, "authored", "data-catalog")
		require.NoError(t, err)
		assert.Equal(t, "skill", row.Type)
		assert.False(t, row.TaskEligible)
	})

	t.Run("search returns ErrNoEmbedder without an embedder", func(t *testing.T) {
		_, err := b.index.search(ctx, "change report", nil, 5)
		assert.ErrorIs(t, err, ErrNoEmbedder)
	})

	t.Run("Uninstall removes bundle + index row", func(t *testing.T) {
		require.NoError(t, b.Uninstall(ctx, "plain-helper"))
		_, err := os.Stat(filepath.Join(root, "plain-helper"))
		assert.True(t, os.IsNotExist(err), "bundle dir must be gone")

		rows, err := b.index.listAll(ctx)
		require.NoError(t, err)
		// change-report + data-catalog remain; plain-helper gone.
		names := make([]string, len(rows))
		for i, r := range rows {
			names[i] = r.Name
		}
		assert.ElementsMatch(t, []string{"change-report", "data-catalog"}, names)
	})
}

// TestStoreReconcileUninstall covers the Store-level accessors that
// the runtime calls (HasDynamic / Reconcile / Uninstall) and confirms
// the dynamic backend takes the local slot when a querier is wired.
func TestStoreReconcileUninstall(t *testing.T) {
	q, agentID := newDynamicTestEngine(t)
	ctx := context.Background()
	root := t.TempDir()

	st := NewSkillStore(Options{
		LocalRoot:      root,
		DynamicQuerier: q,
		AgentID:        agentID,
	})
	require.True(t, st.HasDynamic(), "querier + agentID should wire the dynamic backend")

	// Drop a bundle on disk, reconcile through the Store, see it in List.
	dir := filepath.Join(root, "change-report")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(changeReportSKILL), 0o644))

	n, err := st.Reconcile(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	skills, err := st.List(ctx)
	require.NoError(t, err)
	require.Len(t, skills, 1)
	assert.Equal(t, "change-report", skills[0].Manifest.Name)

	require.NoError(t, st.Uninstall(ctx, "change-report"))
	skills, err = st.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, skills)
}

// TestStoreSyncDynamic covers the hub install path: bundles in a
// separate hub dir get INDEXED into `skills` per the install set
// (no copy — per-source dirs stay), source="hub", bundle_path → the
// hub dir; Get resolves content from there; catalogs relink.
func TestStoreSyncDynamic(t *testing.T) {
	q, agentID := newDynamicTestEngine(t)
	ctx := context.Background()
	localRoot := t.TempDir()
	hubDir := t.TempDir()

	// Three bundles materialised in the hub dir (as phaseBundledSkills
	// would): a catalog, its member recipe, and an unrelated skill.
	for name, body := range map[string]string{
		"change-report": changeReportSKILL,
		"data-catalog":  dataCatalogSKILL,
		"plain-helper":  plainHelperSKILL,
	} {
		dir := filepath.Join(hubDir, name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644))
	}

	st := NewSkillStore(Options{
		LocalRoot:      localRoot,
		DynamicQuerier: q,
		AgentID:        agentID,
	})
	require.True(t, st.HasDynamic())

	// Install-set names only the catalog + its member; plain-helper is
	// excluded (config is authoritative when declared).
	n, err := st.SyncDynamic(ctx, hubDir, []string{"change-report", "data-catalog"}, true)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	skills, err := st.List(ctx)
	require.NoError(t, err)
	names := make([]string, len(skills))
	for i, s := range skills {
		names[i] = s.Manifest.Name
	}
	assert.ElementsMatch(t, []string{"change-report", "data-catalog"}, names,
		"only the install-set subset should be indexed; plain-helper excluded")

	// bundle_path points at the hub dir, not the (empty) local dir.
	row, err := st.dynamic.index.getByName(ctx, "data-catalog")
	require.NoError(t, err)
	assert.Equal(t, "hub", row.Source)
	assert.True(t, strings.HasPrefix(row.BundlePath, hubDir), "bundle_path must reference the hub dir: %s", row.BundlePath)

	// Get reads the full bundle (body) from the hub dir via bundle_path.
	got, err := st.Get(ctx, "change-report")
	require.NoError(t, err)
	assert.Contains(t, string(got.Manifest.Body), "change-report recipe body")

	// Absent install-set (declared=false) installs every hub bundle —
	// plain-helper now joins the index.
	n, err = st.SyncDynamic(ctx, hubDir, nil, false)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, 3)
	skills, err = st.List(ctx)
	require.NoError(t, err)
	assert.Len(t, skills, 3, "declared=false installs all hub bundles")

	// ApplyPins is authoritative: data-catalog pinned, the rest cleared.
	require.NoError(t, st.ApplyPins(ctx, []string{"data-catalog"}))
	pinByName := func() map[string]bool {
		rows, err := st.dynamic.index.listAll(ctx)
		require.NoError(t, err)
		out := map[string]bool{}
		for _, r := range rows {
			out[r.Name] = r.Pin
		}
		return out
	}
	pins := pinByName()
	assert.True(t, pins["data-catalog"], "data-catalog should be pinned")
	assert.False(t, pins["change-report"], "change-report should not be pinned")
	assert.False(t, pins["plain-helper"], "plain-helper should not be pinned")

	// Re-applying with a different set un-pins the old one (authoritative).
	require.NoError(t, st.ApplyPins(ctx, []string{"change-report"}))
	pins = pinByName()
	assert.True(t, pins["change-report"], "change-report now pinned")
	assert.False(t, pins["data-catalog"], "data-catalog un-pinned")
}

// TestStoreNoQuerier_FallsBackToLocal confirms the consolidation
// guard: without a querier the LocalRoot stays the plain dirBackend
// and HasDynamic reports false.
func TestStoreNoQuerier_FallsBackToLocal(t *testing.T) {
	st := NewSkillStore(Options{LocalRoot: t.TempDir()})
	assert.False(t, st.HasDynamic())
	_, err := st.Reconcile(context.Background())
	require.NoError(t, err)
	assert.ErrorIs(t, st.Uninstall(context.Background(), "x"), ErrUnsupportedBackend)
}

