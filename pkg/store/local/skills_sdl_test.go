//go:build duckdb_arrow

package local

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hugr-lab/hugen/pkg/store/queries"
)

// TestSkillsSDL_Compiles_And_RoundTrips is the Phase-6.2.db schema
// boot-check: it provisions a hub.db with the dynamic-skills tables,
// boots the engine with @embeddings ENABLED (VectorSize>0 +
// EmbedderModel set) so the directive is exercised, then round-trips
// a `skills` row + a `skill_links` edge through the auto-generated
// GraphQL ops. No live embedder is registered — inserts omit the
// `summary:` arg, so embedding generation never runs (mirrors the
// !embedderEnabled write path). Catches SDL directive typos
// (@field_references / junction wiring / @embeddings) before the
// store layer is built on top.
func TestSkillsSDL_Compiles_And_RoundTrips(t *testing.T) {
	svc := pinTestEngine(t, "gemma-embedding", 384)
	ctx := context.Background()
	const agentID = "agt_ag01"

	// Insert a catalog skill + a member task skill.
	for _, id := range []string{"skl-cat01", "skl-task01"} {
		require.NoError(t, queries.RunMutation(ctx, svc,
			`mutation ($data: hub_agent_db_skills_mut_input_data!) {
				hub { agent { db { insert_skills(data: $data) { id } } } }
			}`,
			map[string]any{"data": map[string]any{
				"id":                id,
				"agent_id":          agentID,
				"shared":            false,
				"name":              id,
				"type":              "catalog",
				"description":       "x",
				"task_eligible":     false,
				"has_inputs_schema": false,
				"metadata":          map[string]any{"hugen": map[string]any{}},
				"pin":               false,
				"source":            "authored",
			}},
		))
	}

	// Link catalog -> task via the junction table.
	require.NoError(t, queries.RunMutation(ctx, svc,
		`mutation ($data: hub_agent_db_skill_links_mut_input_data!) {
			hub { agent { db { insert_skill_links(data: $data) { source_id } } } }
		}`,
		map[string]any{"data": map[string]any{
			"agent_id":  agentID,
			"source_id": "skl-cat01",
			"target_id": "skl-task01",
			"relation":  "catalog_member",
		}},
	))

	// Append a skill_log impression.
	require.NoError(t, queries.RunMutation(ctx, svc,
		`mutation ($data: hub_agent_db_skill_log_mut_input_data!) {
			hub { agent { db { insert_skill_log(data: $data) { id } } } }
		}`,
		map[string]any{"data": map[string]any{
			"id": "slog-1", "skill_id": "skl-cat01", "agent_id": agentID, "event": "shown",
		}},
	))

	// Read the catalog back WITH its outgoing_links relation resolving
	// through the junction table (visibility-controllable junction, not
	// is_m2m) — proves the two @field_references wire up.
	type linkRow struct {
		TargetID string `json:"target_id"`
		Relation string `json:"relation"`
	}
	type skillRow struct {
		ID            string    `json:"id"`
		Name          string    `json:"name"`
		Type          string    `json:"type"`
		Source        string    `json:"source"`
		TaskEligible  bool      `json:"task_eligible"`
		OutgoingLinks []linkRow `json:"outgoing_links"`
	}
	rows, err := queries.RunQuery[[]skillRow](ctx, svc,
		`query ($id: String!) {
			hub { agent { db {
				skills(filter: {id: {eq: $id}}) {
					id name type source task_eligible
					outgoing_links { target_id relation }
				}
			}}}
		}`,
		map[string]any{"id": "skl-cat01"},
		"hub.agent.db.skills",
	)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "catalog", rows[0].Type)
	require.Equal(t, "authored", rows[0].Source)
	require.Len(t, rows[0].OutgoingLinks, 1)
	require.Equal(t, "skl-task01", rows[0].OutgoingLinks[0].TargetID)
	require.Equal(t, "catalog_member", rows[0].OutgoingLinks[0].Relation)
}
