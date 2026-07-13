package skill

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/store/queries"
)

// ErrNoEmbedder is returned by the dynamic index's semantic search
// when no embedder is wired, so the discovery caller can fall back
// to keyword listing. Mirrors pkg/session/store.ErrNoEmbedder; kept
// local so pkg/skill stays free of a cross-domain import.
var ErrNoEmbedder = errors.New("skill: no embedder attached")

// SearchOpts are the optional structural prefilters applied alongside
// the semantic rank in Store.Search / SkillManager.Search.
type SearchOpts struct {
	// TaskEligible, when non-nil & true, restricts results to
	// task-runnable skills.
	TaskEligible *bool
	// Limit caps the ranked result set. 0 → backend default.
	Limit int
}

// skillRow is the wire shape of a `skills` index row. `metadata`
// comes back as a JSON string from DuckDB (Arrow utf8) and a JSONB
// map from Postgres — we scan it as a string then JSON-decode the
// full manifest from it (see manifestFromMetadata). The scalar /
// array columns are denormalised from that same manifest at write
// time for cheap structural WHERE.
type skillRow struct {
	ID              string   `json:"id"`
	AgentID         string   `json:"agent_id"`
	Shared          bool     `json:"shared"`
	Name            string   `json:"name"`
	Type            string   `json:"type"`
	Description     string   `json:"description,omitempty"`
	TaskEligible    bool     `json:"task_eligible"`
	TaskKind        string   `json:"task_kind,omitempty"`
	Keywords        []string `json:"keywords,omitempty"`
	TierCompat      []string `json:"tier_compat,omitempty"`
	HasInputsSchema bool     `json:"has_inputs_schema"`
	Metadata        string   `json:"metadata"`
	Pin             bool     `json:"pin"`
	Source          string   `json:"source"`
	Version         string   `json:"version,omitempty"`
	ContentHash     string   `json:"content_hash,omitempty"`
	BundlePath      string   `json:"bundle_path,omitempty"`
	Owner           string   `json:"owner,omitempty"`
}

// skillRowProjection is the column set every skills-read path
// projects. Kept as a constant so call sites stay in sync.
const skillRowProjection = `
	id agent_id shared name type description
	task_eligible task_kind keywords tier_compat has_inputs_schema
	metadata pin source version content_hash bundle_path owner
`

// dynamicIndex is the GraphQL-backed index layer over the `skills`
// table — every operation runs through hub.db via queries.RunQuery /
// queries.RunMutation, mirroring pkg/scheduler/store.LocalTaskStore.
// All reads/writes are sliced by AgentID (the multi-tenant boundary).
type dynamicIndex struct {
	querier         types.Querier
	agentID         string
	embedderEnabled bool
}

// listAll returns every index row for the agent, alpha-sorted by
// name. The Backend.List discovery path + the startup reconcile both
// consume it.
func (x *dynamicIndex) listAll(ctx context.Context) ([]skillRow, error) {
	rows, err := queries.RunQuery[[]skillRow](ctx, x.querier,
		`query ($agent: String!) {
			hub { agent { db {
				skills(
					filter: {agent_id: {eq: $agent}},
					order_by: [{field: "name", direction: ASC}]
				) {`+skillRowProjection+`}
			}}}
		}`,
		map[string]any{"agent": x.agentID},
		"hub.agent.db.skills",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	return rows, nil
}

// getRowByName looks up the row for (agent_id, source, name) — the
// identity tuple update-at-start matches on, so upsert keeps the
// minted `id` across version bumps. Returns a zero row + nil when
// absent.
func (x *dynamicIndex) getRowByName(ctx context.Context, source, name string) (skillRow, error) {
	rows, err := queries.RunQuery[[]skillRow](ctx, x.querier,
		`query ($agent: String!, $source: String!, $name: String!) {
			hub { agent { db {
				skills(
					filter: {agent_id: {eq: $agent}, source: {eq: $source}, name: {eq: $name}},
					limit: 1
				) {`+skillRowProjection+`}
			}}}
		}`,
		map[string]any{"agent": x.agentID, "source": source, "name": name},
		"hub.agent.db.skills",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return skillRow{}, nil
		}
		return skillRow{}, err
	}
	if len(rows) == 0 {
		return skillRow{}, nil
	}
	return rows[0], nil
}

// sourcePrecedence orders same-name rows across sources for resolution: a
// lower rank wins. `authored` (the agent's own skill:save) shadows the
// admin-delivered `hub` bundle — mirroring local-shadows-hub in the tier
// model (spec-skills-distribution §1). Unknown sources sort last.
var sourcePrecedence = map[string]int{
	"authored":  0,
	"local":     1,
	"hub":       2,
	"catalogue": 3,
	"memory":    4,
	"file":      5,
}

func sourceRank(source string) int {
	if r, ok := sourcePrecedence[source]; ok {
		return r
	}
	return 100
}

// getByName resolves the full index row for (agent_id, name), any source.
// When the same name exists under multiple sources it resolves
// deterministically by [sourcePrecedence] (authored > hub), not by an
// arbitrary limit-1 pick. Returns a zero row + nil when absent. Used by Get
// (resolve bundle_path) + IndexBundle (hash-skip check) where the source
// isn't known up front.
func (x *dynamicIndex) getByName(ctx context.Context, name string) (skillRow, error) {
	rows, err := queries.RunQuery[[]skillRow](ctx, x.querier,
		`query ($agent: String!, $name: String!) {
			hub { agent { db {
				skills(filter: {agent_id: {eq: $agent}, name: {eq: $name}}) {`+skillRowProjection+`}
			}}}
		}`,
		map[string]any{"agent": x.agentID, "name": name},
		"hub.agent.db.skills",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return skillRow{}, nil
		}
		return skillRow{}, err
	}
	if len(rows) == 0 {
		return skillRow{}, nil
	}
	best := rows[0]
	for _, r := range rows[1:] {
		if sourceRank(r.Source) < sourceRank(best.Source) {
			best = r
		}
	}
	return best, nil
}

// upsert indexes a manifest given the PRE-FETCHED existing row for
// (agent_id, source, name) (zero row = absent). Update-in-place when
// existing.ID != "" (keeping the minted id), else insert a fresh row.
// Hugr exposes no native upsert, so callers fetch `existing` via
// getRowByName ONCE and pass it here — avoiding a second lookup the
// caller already did for its hash-skip check (mirrors LocalTaskStore
// semantics). Returns the row id. When the embedder is enabled, the
// manifest description is passed as `summary:` so Hugr regenerates the
// description_vec server-side.
//
// A unique index on (agent_id, source, name) makes a concurrent
// double-insert (the TOCTOU between the caller's fetch and this insert)
// fail loud rather than silently duplicate.
func (x *dynamicIndex) upsert(ctx context.Context, m Manifest, source, bundlePath, contentHash string, existing skillRow) (string, error) {
	metaJSON, err := manifestMetadataMap(m)
	if err != nil {
		return "", fmt.Errorf("skill: project manifest %q: %w", m.Name, err)
	}

	id := existing.ID
	if id == "" {
		id = newSkillID()
	}

	data := map[string]any{
		"id":                id,
		"agent_id":          x.agentID,
		"shared":            existing.Shared, // preserve sharing flag across updates
		"name":              m.Name,
		"type":              skillType(m),
		"description":       m.Description,
		"task_eligible":     m.Hugen.Task.Eligible,
		"has_inputs_schema": len(m.Hugen.Task.InputsSchema) > 0,
		"metadata":          metaJSON,
		"pin":               existing.Pin, // preserve advertise-pin across updates
		"source":            source,
		"bundle_path":       bundlePath,
	}
	if m.Hugen.Task.Kind != "" {
		data["task_kind"] = m.Hugen.Task.Kind
	}
	if kw := m.Hugen.Mission.Keywords; len(kw) > 0 {
		data["keywords"] = kw
	}
	if tc := m.EffectiveTierCompatibility(); len(tc) > 0 {
		data["tier_compat"] = tc
	}
	if contentHash != "" {
		data["content_hash"] = contentHash
	}

	// When the embedder is wired, pass the description as `summary:`
	// so Hugr regenerates description_vec server-side. The `$summary`
	// variable is declared ONLY when used — mirrors the session-store
	// notepad insert (two mutation shapes), avoiding a declared-but-
	// unused variable.
	withSummary := x.embedderEnabled && m.Description != ""

	if existing.ID == "" {
		vars := map[string]any{"data": data}
		sig, call := "$data: hub_agent_db_skills_mut_input_data!", "insert_skills(data: $data)"
		if withSummary {
			vars["summary"] = m.Description
			sig = "$data: hub_agent_db_skills_mut_input_data!, $summary: String"
			call = "insert_skills(data: $data, summary: $summary)"
		}
		mutation := fmt.Sprintf(`mutation (%s) { hub { agent { db { %s { id } } } } }`, sig, call)
		if err := queries.RunMutation(ctx, x.querier, mutation, vars); err != nil {
			return "", fmt.Errorf("skill: insert %q: %w", m.Name, err)
		}
		return id, nil
	}

	// Update in place. The id / agent_id / source are the match key,
	// not patch targets — drop them from the data payload.
	delete(data, "id")
	delete(data, "agent_id")
	delete(data, "source")
	vars := map[string]any{"id": id, "data": data}
	sig := "$id: String!, $data: hub_agent_db_skills_mut_data!"
	call := "update_skills(filter: {id: {eq: $id}}, data: $data)"
	if withSummary {
		vars["summary"] = m.Description
		sig = "$id: String!, $data: hub_agent_db_skills_mut_data!, $summary: String"
		call = "update_skills(filter: {id: {eq: $id}}, data: $data, summary: $summary)"
	}
	mutation := fmt.Sprintf(`mutation (%s) { hub { agent { db { %s { affected_rows } } } } }`, sig, call)
	if err := queries.RunMutation(ctx, x.querier, mutation, vars); err != nil {
		return "", fmt.Errorf("skill: update %q: %w", m.Name, err)
	}
	return id, nil
}

// setPinAll bulk-sets the advertise-pin flag on EVERY indexed skill
// for the agent in one mutation (filter on agent_id only). The pin
// column is preserved across upserts, so this config-driven path is
// the only writer.
func (x *dynamicIndex) setPinAll(ctx context.Context, pin bool) error {
	return queries.RunMutation(ctx, x.querier,
		`mutation ($agent: String!, $data: hub_agent_db_skills_mut_data!) {
			hub { agent { db {
				update_skills(filter: {agent_id: {eq: $agent}}, data: $data) { affected_rows }
			}}}
		}`,
		map[string]any{"agent": x.agentID, "data": map[string]any{"pin": pin}},
	)
}

// setPinForNames bulk-sets the pin flag on the named skills in one
// mutation via an `in` filter (Hugr collapses the list into a single
// statement — see filtering docs "Use IN Instead of Multiple OR").
func (x *dynamicIndex) setPinForNames(ctx context.Context, names []string, pin bool) error {
	if len(names) == 0 {
		return nil
	}
	return queries.RunMutation(ctx, x.querier,
		`mutation ($agent: String!, $names: [String!], $data: hub_agent_db_skills_mut_data!) {
			hub { agent { db {
				update_skills(filter: {agent_id: {eq: $agent}, name: {in: $names}}, data: $data) { affected_rows }
			}}}
		}`,
		map[string]any{"agent": x.agentID, "names": names, "data": map[string]any{"pin": pin}},
	)
}

// deleteByName removes the index row for (agent_id, name), source-blind.
// No-op when absent.
func (x *dynamicIndex) deleteByName(ctx context.Context, name string) error {
	return queries.RunMutation(ctx, x.querier,
		`mutation ($agent: String!, $name: String!) {
			hub { agent { db {
				delete_skills(filter: {agent_id: {eq: $agent}, name: {eq: $name}}) { affected_rows }
			}}}
		}`,
		map[string]any{"agent": x.agentID, "name": name},
	)
}

// deleteBySourceName removes the index row for the exact identity tuple
// (agent_id, source, name) — the tier-aware delete used by Uninstall so a
// removal of the `hub` copy never also drops an `authored` same-name row.
func (x *dynamicIndex) deleteBySourceName(ctx context.Context, source, name string) error {
	return queries.RunMutation(ctx, x.querier,
		`mutation ($agent: String!, $source: String!, $name: String!) {
			hub { agent { db {
				delete_skills(filter: {agent_id: {eq: $agent}, source: {eq: $source}, name: {eq: $name}}) { affected_rows }
			}}}
		}`,
		map[string]any{"agent": x.agentID, "source": source, "name": name},
	)
}

// search runs discovery over the index. When the embedder is wired it
// is the PRIMARY path — a semantic top-K via Hugr's `semantic:` arg.
// Without an embedder it returns ErrNoEmbedder so the caller degrades
// to keyword listing (the notepad precedent). `taskEligible` is an
// optional structural prefilter applied alongside the rank.
func (x *dynamicIndex) search(ctx context.Context, query string, taskEligible *bool, limit int) ([]skillRow, error) {
	if !x.embedderEnabled {
		return nil, ErrNoEmbedder
	}
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("skill: search requires a non-empty query")
	}
	if limit <= 0 {
		limit = 10
	}
	filter := map[string]any{"agent_id": map[string]any{"eq": x.agentID}}
	if taskEligible != nil {
		filter["task_eligible"] = map[string]any{"eq": *taskEligible}
	}
	rows, err := queries.RunQuery[[]skillRow](ctx, x.querier,
		`query ($filter: hub_agent_db_skills_filter, $semantic: SemanticSearchInput) {
			hub { agent { db {
				skills(filter: $filter, semantic: $semantic) {`+skillRowProjection+`}
			}}}
		}`,
		map[string]any{
			"filter":   filter,
			"semantic": map[string]any{"query": query, "limit": limit},
		},
		"hub.agent.db.skills",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	return rows, nil
}

// rankedSkillRow parses the db-2 recall+counts projection: the semantic
// candidate plus its skill_log usage bucketed by event (one row per
// event with its _rows_count).
type rankedSkillRow struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	TaskEligible bool   `json:"task_eligible"`
	Log          []struct {
		Key struct {
			Event string `json:"event"`
		} `json:"key"`
		Aggregations struct {
			RowsCount int `json:"_rows_count"`
		} `json:"aggregations"`
	} `json:"log_bucket_aggregation"`
}

// recallProjection selects exactly what the bandit needs — the candidate
// fields + the per-event usage tallies via the auto-generated
// log_bucket_aggregation (the `log` reverse relation grouped by event).
const recallProjection = `id name description task_eligible
	log_bucket_aggregation { key { event } aggregations { _rows_count } }`

// recallRanked runs the db-2 recall+counts query in ONE round-trip via
// two aliased selections:
//
//   - dynamic — a SEMANTIC search over non-pinned skills (the bandit's
//     candidate pool, relevance-gated to `limit`); the caller Thompson-
//     ranks these;
//   - pinned — ALL pinned skills (operator always-advertise, bandit-
//     bypass), regardless of topic relevance.
//
// Both carry their shown/used tallies via log_bucket_aggregation, so the
// whole advertise input is one query. Returns ErrNoEmbedder when no
// embedder is wired (caller falls back to keyword/List).
func (x *dynamicIndex) recallRanked(ctx context.Context, query string, limit int) (dynamic, pinned []RecallCandidate, err error) {
	if !x.embedderEnabled {
		return nil, nil, ErrNoEmbedder
	}
	if strings.TrimSpace(query) == "" {
		return nil, nil, fmt.Errorf("skill: recall requires a non-empty query")
	}
	if limit <= 0 {
		limit = 100
	}
	agentEq := map[string]any{"eq": x.agentID}
	// One query, two aliased selections — RunQuery scans a single path, so
	// run it directly and ScanData each alias off the same response.
	resp, err := x.querier.Query(ctx,
		`query ($dyn: hub_agent_db_skills_filter, $pin: hub_agent_db_skills_filter, $semantic: SemanticSearchInput) {
			hub { agent { db {
				dynamic: skills(filter: $dyn, semantic: $semantic) {`+recallProjection+`}
				pinned: skills(filter: $pin) {`+recallProjection+`}
			}}}
		}`,
		map[string]any{
			"dyn":      map[string]any{"agent_id": agentEq, "pin": map[string]any{"eq": false}},
			"pin":      map[string]any{"agent_id": agentEq, "pin": map[string]any{"eq": true}},
			"semantic": map[string]any{"query": query, "limit": limit},
		},
	)
	if err != nil {
		return nil, nil, fmt.Errorf("skill: recall query: %w", err)
	}
	defer resp.Close()
	if err := resp.Err(); err != nil {
		return nil, nil, fmt.Errorf("skill: recall graphql: %w", err)
	}
	scanAlias := func(path string) ([]rankedSkillRow, error) {
		var rows []rankedSkillRow
		if serr := resp.ScanData(path, &rows); serr != nil {
			if errors.Is(serr, types.ErrWrongDataPath) || errors.Is(serr, types.ErrNoData) {
				return nil, nil
			}
			return nil, serr
		}
		return rows, nil
	}
	dynRows, err := scanAlias("hub.agent.db.dynamic")
	if err != nil {
		return nil, nil, err
	}
	pinRows, err := scanAlias("hub.agent.db.pinned")
	if err != nil {
		return nil, nil, err
	}
	return candidatesFromRows(dynRows), candidatesFromRows(pinRows), nil
}

// candidatesFromRows projects the index rows + their event buckets into
// the public RecallCandidate shape.
func candidatesFromRows(rows []rankedSkillRow) []RecallCandidate {
	out := make([]RecallCandidate, 0, len(rows))
	for _, r := range rows {
		c := RecallCandidate{ID: r.ID, Name: r.Name, Description: r.Description, TaskEligible: r.TaskEligible}
		for _, b := range r.Log {
			switch b.Key.Event {
			case SkillLogShown:
				c.Shown = b.Aggregations.RowsCount
			case SkillLogUsed:
				c.Used = b.Aggregations.RowsCount
			}
		}
		out = append(out, c)
	}
	return out
}

// getIDByName resolves a skill's id from its name within the agent
// scope (any source). Returns "" + nil when absent — used to resolve
// catalog / member ids for link writes + reads.
func (x *dynamicIndex) getIDByName(ctx context.Context, name string) (string, error) {
	rows, err := queries.RunQuery[[]skillRow](ctx, x.querier,
		`query ($agent: String!, $name: String!) {
			hub { agent { db {
				skills(filter: {agent_id: {eq: $agent}, name: {eq: $name}}, limit: 1) { id }
			}}}
		}`,
		map[string]any{"agent": x.agentID, "name": name},
		"hub.agent.db.skills",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return "", nil
		}
		return "", err
	}
	if len(rows) == 0 {
		return "", nil
	}
	return rows[0].ID, nil
}

// newSkillLogID mints a skill_log row id (`slog-<hex>`).
func newSkillLogID() string {
	var b [9]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "slog-fallback"
	}
	return "slog-" + hex.EncodeToString(b[:])
}

// logSkillEvents appends one append-only skill_log row per skill id for
// the given event (shown / loaded / used). Empty ids are skipped — system
// / inline skills have no index row, and skill_log.skill_id is an FK into
// skills. Used by the `shown` path, which already holds index ids from
// List. Best-effort + accumulate. Insert-only (append-only constitution).
func (x *dynamicIndex) logSkillEvents(ctx context.Context, skillIDs []string, event, sessionID string) error {
	var errs []error
	for _, sid := range skillIDs {
		if err := x.insertSkillLog(ctx, sid, event, sessionID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// insertSkillLog writes one append-only skill_log row. Empty id is a
// no-op (non-indexed skill). sessionID optional.
func (x *dynamicIndex) insertSkillLog(ctx context.Context, skillID, event, sessionID string) error {
	if skillID == "" {
		return nil
	}
	data := map[string]any{
		"id":       newSkillLogID(),
		"skill_id": skillID,
		"agent_id": x.agentID,
		"event":    event,
	}
	if sessionID != "" {
		data["session_id"] = sessionID
	}
	if err := queries.RunMutation(ctx, x.querier,
		`mutation ($data: hub_agent_db_skill_log_mut_input_data!) {
			hub { agent { db { insert_skill_log(data: $data) { id } } } }
		}`,
		map[string]any{"data": data},
	); err != nil {
		return fmt.Errorf("skill: log %s/%s: %w", event, skillID, err)
	}
	return nil
}

// --- manifest <-> row helpers ---

// skillType classifies a manifest into the coarse `type` column used
// for the structural discovery prefilter.
func skillType(m Manifest) string {
	if m.Hugen.Task.Eligible {
		return "recipe"
	}
	return "skill"
}

// manifestMetadataMap marshals the full manifest frontmatter (name +
// description + license + allowed-tools + the metadata map carrying
// metadata.hugen.*) into the map[string]any Hugr's JSON input mapper
// consumes. Reconstructed losslessly by manifestFromMetadata — so the
// DB index serves a fully-typed manifest with no SKILL.md read.
func manifestMetadataMap(m Manifest) (map[string]any, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// manifestFromMetadata reconstructs a Manifest from the JSON stored in
// the `metadata` column. Re-runs extractHugen so the typed Hugen
// projection (mission roles / task inputs_schema / tier set) is
// populated WITHOUT touching disk. Body / Raw stay empty — actual
// content loads lazily from the bundle via Backend.Get.
func manifestFromMetadata(raw string) (Manifest, error) {
	var m Manifest
	if raw == "" || raw == "null" {
		return m, nil
	}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return m, fmt.Errorf("skill: decode metadata: %w", err)
	}
	hugen, err := extractHugen(m.Metadata)
	if err != nil {
		return m, fmt.Errorf("skill: re-extract hugen: %w", err)
	}
	m.Hugen = hugen
	return m, nil
}

// rowToSkill reconstructs a discovery-facing Skill from an index row.
// Manifest comes from the metadata JSON; FS / Root point at the
// on-disk bundle for lazy content loads.
func rowToSkill(r skillRow) (Skill, error) {
	m, err := manifestFromMetadata(r.Metadata)
	if err != nil {
		return Skill{}, err
	}
	// The metadata JSON is authoritative for the manifest, but the
	// denormalised name/description columns win if the JSON was thin.
	if m.Name == "" {
		m.Name = r.Name
	}
	if m.Description == "" {
		m.Description = r.Description
	}
	s := Skill{Manifest: m, Origin: OriginDynamic, ID: r.ID}
	if r.BundlePath != "" {
		s.FS = os.DirFS(r.BundlePath)
		s.Root = r.BundlePath
	}
	return s, nil
}

// newSkillID mints the stable prefixed id (`skl-<hex>`) used as the
// immutable PK. Minted once at first insert; preserved across updates.
func newSkillID() string {
	var b [9]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is effectively impossible; fall back to
		// a name-free token so the insert still gets a unique id.
		return "skl-fallback"
	}
	return "skl-" + hex.EncodeToString(b[:])
}

// bundleHash returns the canonical whole-bundle hash (BundleHash: sha256
// over every non-dotfile in the dir, sorted by relpath). It is the single
// drift signal shared by the seed sentinel, the ledger, the catalog
// compare, and the `skills.content_hash` column (spec-skills-distribution
// §2). Replaces the former SKILL.md-only hash, which was blind to script
// changes. Returns "" on a read error (callers treat "" as "unknown, do
// not skip"). One-time cost on the first boot after this lands: every
// indexed bundle re-hashes to a whole-tree value and re-indexes once.
func bundleHash(dir string) string {
	h, err := BundleHash(os.DirFS(dir))
	if err != nil {
		return ""
	}
	return h
}

// --- dynamic backend (dir content + DB index) ---

// dynamicBackend is the Phase-6.2.db writable user source. Content
// lives on disk as bundles (the dirBackend write path, reused
// verbatim for atomicity); the DB index serves discovery. List reads
// the DB index (metadata only, no disk); Get reads the bundle from
// disk (full manifest incl. body) — the load path needs the prose.
// Publish writes both. Consolidates OriginLocal.
type dynamicBackend struct {
	dir *dirBackend
	// hubRoot is the hub-tier install dir (${state}/skills/hub). Held so a
	// tier-aware Uninstall of a hub bundle can reach its ledger + on-disk dir
	// (the writable `dir.root` is the authored/local tier). Empty when no hub
	// tier is configured.
	hubRoot string
	index   *dynamicIndex
	log     *slog.Logger
}

// newDynamicBackend wires the on-disk bundle root + the DB index.
func newDynamicBackend(root, hubRoot string, q types.Querier, agentID string, embedderEnabled bool, log *slog.Logger) *dynamicBackend {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &dynamicBackend{
		dir:     &dirBackend{origin: OriginDynamic, root: root, writable: true},
		hubRoot: hubRoot,
		index:   &dynamicIndex{querier: q, agentID: agentID, embedderEnabled: embedderEnabled},
		log:     log,
	}
}

func (b *dynamicBackend) Origin() Origin { return OriginDynamic }

// LogSkillEvents appends append-only skill_log rows for the given skill
// ids + event (shown / loaded / used). Empty ids skipped inside the
// index writer. Phase 6.2.db-2.
func (b *dynamicBackend) LogSkillEvents(ctx context.Context, skillIDs []string, event, sessionID string) error {
	return b.index.logSkillEvents(ctx, skillIDs, event, sessionID)
}

// RecallRanked runs the db-2 recall+counts query in one round-trip,
// returning the semantic non-pinned candidate pool (caller Thompson-
// ranks) AND the always-advertise pinned set, both with shown/used
// tallies. ErrNoEmbedder when no embedder is wired. Phase 6.2.db-2.
func (b *dynamicBackend) RecallRanked(ctx context.Context, query string, limit int) (dynamic, pinned []RecallCandidate, err error) {
	return b.index.recallRanked(ctx, query, limit)
}

// List reads the DB index — fast, metadata-only, no disk walk. The
// returned skills carry FS handles for lazy content, but their
// manifests are served straight from the index's metadata column.
func (b *dynamicBackend) List(ctx context.Context) ([]Skill, error) {
	rows, err := b.index.listAll(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Skill, 0, len(rows))
	var errs []error
	for _, r := range rows {
		s, err := rowToSkill(r)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.Name, err))
			continue
		}
		out = append(out, s)
	}
	return out, errors.Join(errs...)
}

// Get reads the bundle from disk — the load path needs the full
// manifest including the prose body, which the index does not carry.
// Resolves the physical location via the index row's bundle_path
// (skills live in per-source dirs — hub / local / …), so a hub skill
// indexed from ${state}/skills/hub loads from there. Falls back to the
// writable dir by name when the skill isn't indexed yet (freshly
// saved, pre-reconcile) or its bundle_path can't be read.
func (b *dynamicBackend) Get(ctx context.Context, name string) (Skill, error) {
	row, err := b.index.getByName(ctx, name)
	if err != nil {
		return Skill{}, err
	}
	if row.ID != "" && row.BundlePath != "" {
		if sk, rerr := b.dir.readSkillDir(row.BundlePath); rerr == nil {
			return sk, nil
		}
	}
	return b.dir.Get(ctx, name)
}

// IndexBundle indexes a single bundle that physically lives at `dir`
// (any per-source location) into the DB index under `source`, with
// bundle_path = dir. Hash-skips when the bundle is already indexed at
// the same content (avoids a redundant re-embed). Returns the row id
// + whether it changed. This is the install primitive — the runtime
// calls it per install-set entry against the hub bundle dir.
func (b *dynamicBackend) IndexBundle(ctx context.Context, dir, source string) (string, bool, error) {
	sk, err := b.dir.readSkillDir(dir)
	if err != nil {
		return "", false, err
	}
	hash := bundleHash(dir)
	// Match on (agent_id, source, name) — the same identity tuple
	// upsert keys on. getByName (any source) would alias a same-named
	// bundle from a DIFFERENT source and skip / index the wrong row,
	// since `name` is unique only within (agent_id, source). One
	// lookup, reused for both the hash-skip and the upsert below.
	existing, err := b.index.getRowByName(ctx, source, sk.Manifest.Name)
	if err != nil {
		return "", false, err
	}
	if existing.ID != "" && hash != "" && existing.ContentHash == hash {
		return existing.ID, false, nil
	}
	id, err := b.index.upsert(ctx, sk.Manifest, source, dir, hash, existing)
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

// Publish writes the bundle to disk (atomic swap via dirBackend) then
// upserts the DB index row + embedding. A disk write that succeeds
// followed by an index failure leaves the bundle reachable via Get +
// recoverable by the next startup reconcile, so we surface the index
// error without rolling back the bundle.
func (b *dynamicBackend) Publish(ctx context.Context, m Manifest, body fs.FS, opts PublishOptions) error {
	if err := b.dir.Publish(ctx, m, body, opts); err != nil {
		return err
	}
	dir := filepath.Join(b.dir.root, m.Name)
	existing, err := b.index.getRowByName(ctx, "authored", m.Name)
	if err != nil {
		return fmt.Errorf("skill: index lookup after publish %q: %w", m.Name, err)
	}
	if _, err := b.index.upsert(ctx, m, "authored", dir, bundleHash(dir), existing); err != nil {
		return fmt.Errorf("skill: index after publish %q: %w", m.Name, err)
	}
	return nil
}

// applyPins reconciles the advertise-pin flag against the authoritative
// pin set: every indexed skill whose name is in pinNames gets pin=true,
// all others pin=false. Only writes rows whose flag actually changes.
// Idempotent — re-running converges. Pin is a discovery signal (db-2
// advertise bypass); db-1 just stores it.
func (b *dynamicBackend) applyPins(ctx context.Context, pinNames []string) error {
	// Two bulk mutations regardless of skill count: reset every row to
	// pin=false, then set the named set to pin=true. Replaces the
	// former read-all + per-changed-row update (N+1) — `nin` isn't a
	// Hugr filter op, so reset-then-set expresses "exactly this set is
	// pinned" without a not-in. Sequential at boot / on a static-config
	// no-op OnUpdate, so the brief all-false window between the two
	// statements is not observable.
	if err := b.index.setPinAll(ctx, false); err != nil {
		return fmt.Errorf("skill: reset pins: %w", err)
	}
	if err := b.index.setPinForNames(ctx, pinNames, true); err != nil {
		return fmt.Errorf("skill: set pins: %w", err)
	}
	b.log.Debug("skill pin: applied", "pinned", pinNames)
	return nil
}

// Uninstall is the tier-aware triple-delete (spec-skills-distribution §3):
// it resolves the skill's index row (authored > hub precedence), then removes
// the DB row by the exact (agent_id, source, name) tuple, the bundle dir at
// the row's own bundle_path (hub or authored root — NOT assumed to be the
// writable root), and, for a hub-tier install, its `.installed.json` ledger
// entry. A `desired`-origin hub install is refused — it is managed by the
// admin desired-set and would be re-installed next reconcile; drop it from
// the set instead. This is the only explicit removal path (bandit hygiene
// demotes but never deletes; reconcile only adds/updates).
//
// Ledger note: Uninstall load-modify-saves the hub ledger, which the
// background reconciler also writes. The window is small (remove is a
// user-initiated one-shot) and both converge on the next pass; serialising
// the two writers is a follow-up.
func (b *dynamicBackend) Uninstall(ctx context.Context, name string) error {
	row, err := b.index.getByName(ctx, name)
	if err != nil {
		return fmt.Errorf("skill: uninstall lookup %q: %w", name, err)
	}
	if row.ID == "" {
		return ErrSkillNotFound
	}
	source := row.Source

	// Load the hub ledger once for hub-tier installs (desired-refusal + the
	// entry delete below).
	var ledger *Ledger
	if source == "hub" && b.hubRoot != "" {
		if l, lerr := LoadLedger(b.hubRoot); lerr == nil {
			ledger = l
			if entry, ok := ledger.Get(name); ok && entry.Origin == InstallDesired {
				return fmt.Errorf("skill %q is managed by the admin desired-set; drop it from the set instead of removing it", name)
			}
		} else {
			b.log.Warn("skill: uninstall ledger read", "name", name, "err", lerr)
		}
	}

	if err := b.index.deleteBySourceName(ctx, source, name); err != nil {
		return fmt.Errorf("skill: uninstall index %q (%s): %w", name, source, err)
	}

	// Remove the bundle at its own path (falls back to the writable root by
	// name when the row carries no bundle_path — an in-memory/legacy row).
	dir := row.BundlePath
	if dir == "" {
		dir = filepath.Join(b.dir.root, name)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("skill: uninstall bundle %q: %w", name, err)
	}

	if ledger != nil {
		ledger.Delete(name)
		if err := ledger.Save(); err != nil {
			b.log.Warn("skill: uninstall ledger save", "name", name, "err", err)
		}
	}
	return nil
}

// indexDir indexes every bundle directly under `root` into the DB
// under `source` (bundle_path = root/<name>). Hash-skips unchanged
// bundles. Returns the count indexed (incl. skipped-unchanged) and a
// joined per-bundle error. Orphan index rows whose bundle is gone are
// left in place — removal is uninstall's job, never a surprise delete.
func (b *dynamicBackend) indexDir(ctx context.Context, root, source string) (int, error) {
	if root == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("skill: index dir %s: %w", root, err)
	}
	n := 0
	var errs []error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if err := ctx.Err(); err != nil {
			return n, err
		}
		if _, _, err := b.IndexBundle(ctx, filepath.Join(root, e.Name()), source); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// Reconcile re-indexes the writable (authored / local) bundle dir into
// the DB at startup. Returns the count of authored bundles indexed.
func (b *dynamicBackend) Reconcile(ctx context.Context) (int, error) {
	return b.indexDir(ctx, b.dir.root, "authored")
}

// installFromDir indexes bundles from a read-only source dir (e.g. the
// materialised hub bundle dir) into the index under `source`, gated by
// the install set:
//
//   - declared == false  — install EVERY bundle in the dir (OOTB: the
//     operator said nothing, ship the full set);
//   - declared == true   — install only the named bundles that exist
//     in the dir (config is authoritative; an
//     empty names list installs nothing).
//
// Returns the count installed/updated.
func (b *dynamicBackend) installFromDir(ctx context.Context, root, source string, names []string, declared bool) (int, error) {
	if root == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("skill: install source %s: %w", root, err)
	}
	onDisk := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			onDisk[e.Name()] = struct{}{}
		}
	}

	var want []string
	if declared {
		want = names // authoritative subset (may be empty → install nothing)
	} else {
		want = make([]string, 0, len(onDisk))
		for name := range onDisk {
			want = append(want, name)
		}
	}

	b.log.Debug("skill install: begin", "source", source, "root", root,
		"declared", declared, "requested", len(want), "on_disk", len(onDisk))
	n := 0
	var errs []error
	for _, name := range want {
		if _, ok := onDisk[name]; !ok {
			b.log.Debug("skill install: skip (not in source dir)", "name", name, "source", source)
			continue // named in config but not present in the source dir
		}
		if err := ctx.Err(); err != nil {
			return n, err
		}
		id, changed, err := b.IndexBundle(ctx, filepath.Join(root, name), source)
		if err != nil {
			b.log.Debug("skill install: FAILED", "name", name, "source", source, "err", err)
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		b.log.Debug("skill install: indexed", "name", name, "source", source, "id", id, "changed", changed)
		n++
	}
	b.log.Debug("skill install: done", "source", source, "indexed", n, "errors", len(errs))
	return n, errors.Join(errs...)
}
