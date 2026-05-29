package skill

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
	// Type filters on the coarse `type` column (catalog / recipe /
	// skill). Empty = any.
	Type string
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
			hub { db { agent {
				skills(
					filter: {agent_id: {eq: $agent}},
					order_by: [{field: "name", direction: ASC}]
				) {`+skillRowProjection+`}
			}}}
		}`,
		map[string]any{"agent": x.agentID},
		"hub.db.agent.skills",
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
			hub { db { agent {
				skills(
					filter: {agent_id: {eq: $agent}, source: {eq: $source}, name: {eq: $name}},
					limit: 1
				) {`+skillRowProjection+`}
			}}}
		}`,
		map[string]any{"agent": x.agentID, "source": source, "name": name},
		"hub.db.agent.skills",
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

// getByName resolves the full index row for (agent_id, name), any
// source. Returns a zero row + nil when absent. Used by Get (resolve
// bundle_path) + IndexBundle (hash-skip check) where source isn't
// known up front.
func (x *dynamicIndex) getByName(ctx context.Context, name string) (skillRow, error) {
	rows, err := queries.RunQuery[[]skillRow](ctx, x.querier,
		`query ($agent: String!, $name: String!) {
			hub { db { agent {
				skills(filter: {agent_id: {eq: $agent}, name: {eq: $name}}, limit: 1) {`+skillRowProjection+`}
			}}}
		}`,
		map[string]any{"agent": x.agentID, "name": name},
		"hub.db.agent.skills",
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

// listCatalogs returns the recipe-catalog index rows (type='catalog')
// for the agent — the relink step's work list.
func (x *dynamicIndex) listCatalogs(ctx context.Context) ([]skillRow, error) {
	rows, err := queries.RunQuery[[]skillRow](ctx, x.querier,
		`query ($agent: String!) {
			hub { db { agent {
				skills(filter: {agent_id: {eq: $agent}, type: {eq: "catalog"}}) {`+skillRowProjection+`}
			}}}
		}`,
		map[string]any{"agent": x.agentID},
		"hub.db.agent.skills",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	return rows, nil
}

// upsert indexes a manifest: lookup by (agent_id, source, name), then
// update-in-place (keeping the minted id) or insert a fresh row.
// Hugr exposes no native upsert, so this is the standard
// lookup+insert/update dance (mirrors LocalTaskStore semantics).
// Returns the row id. When the embedder is enabled, the manifest
// description is passed as `summary:` so Hugr regenerates the
// description_vec server-side.
func (x *dynamicIndex) upsert(ctx context.Context, m Manifest, source, bundlePath, contentHash string) (string, error) {
	existing, err := x.getRowByName(ctx, source, m.Name)
	if err != nil {
		return "", fmt.Errorf("skill: index lookup %q: %w", m.Name, err)
	}

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
		sig, call := "$data: hub_db_skills_mut_input_data!", "insert_skills(data: $data)"
		if withSummary {
			vars["summary"] = m.Description
			sig = "$data: hub_db_skills_mut_input_data!, $summary: String"
			call = "insert_skills(data: $data, summary: $summary)"
		}
		mutation := fmt.Sprintf(`mutation (%s) { hub { db { agent { %s { id } } } } }`, sig, call)
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
	sig := "$id: String!, $data: hub_db_skills_mut_data!"
	call := "update_skills(filter: {id: {eq: $id}}, data: $data)"
	if withSummary {
		vars["summary"] = m.Description
		sig = "$id: String!, $data: hub_db_skills_mut_data!, $summary: String"
		call = "update_skills(filter: {id: {eq: $id}}, data: $data, summary: $summary)"
	}
	mutation := fmt.Sprintf(`mutation (%s) { hub { db { agent { %s { affected_rows } } } } }`, sig, call)
	if err := queries.RunMutation(ctx, x.querier, mutation, vars); err != nil {
		return "", fmt.Errorf("skill: update %q: %w", m.Name, err)
	}
	return id, nil
}

// deleteByName removes the index row for (agent_id, name). Uninstall
// pairs this with the on-disk bundle removal. No-op when absent.
func (x *dynamicIndex) deleteByName(ctx context.Context, name string) error {
	return queries.RunMutation(ctx, x.querier,
		`mutation ($agent: String!, $name: String!) {
			hub { db { agent {
				delete_skills(filter: {agent_id: {eq: $agent}, name: {eq: $name}}) { affected_rows }
			}}}
		}`,
		map[string]any{"agent": x.agentID, "name": name},
	)
}

// search runs discovery over the index. When the embedder is wired it
// is the PRIMARY path — a semantic top-K via Hugr's `semantic:` arg.
// Without an embedder it returns ErrNoEmbedder so the caller degrades
// to keyword listing (the notepad precedent). `taskEligible` / `typ`
// are optional structural prefilters applied alongside the rank.
func (x *dynamicIndex) search(ctx context.Context, query string, taskEligible *bool, typ string, limit int) ([]skillRow, error) {
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
	if typ != "" {
		filter["type"] = map[string]any{"eq": typ}
	}
	rows, err := queries.RunQuery[[]skillRow](ctx, x.querier,
		`query ($filter: hub_db_skills_filter, $semantic: SemanticSearchInput) {
			hub { db { agent {
				skills(filter: $filter, semantic: $semantic) {`+skillRowProjection+`}
			}}}
		}`,
		map[string]any{
			"filter":   filter,
			"semantic": map[string]any{"query": query, "limit": limit},
		},
		"hub.db.agent.skills",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	return rows, nil
}

// getIDByName resolves a skill's id from its name within the agent
// scope (any source). Returns "" + nil when absent — used to resolve
// catalog / member ids for link writes + reads.
func (x *dynamicIndex) getIDByName(ctx context.Context, name string) (string, error) {
	rows, err := queries.RunQuery[[]skillRow](ctx, x.querier,
		`query ($agent: String!, $name: String!) {
			hub { db { agent {
				skills(filter: {agent_id: {eq: $agent}, name: {eq: $name}}, limit: 1) { id }
			}}}
		}`,
		map[string]any{"agent": x.agentID, "name": name},
		"hub.db.agent.skills",
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

// replaceLinks sets the (source_id, relation) edge set to exactly
// targetIDs: delete the existing edges for that pair, then insert the
// new ones. Idempotent — re-running reconcile converges on the
// current membership (handles added / removed members). The
// composite PK (agent_id, source_id, target_id, relation) means a
// re-insert without the delete would collide, so the delete is
// mandatory.
func (x *dynamicIndex) replaceLinks(ctx context.Context, sourceID, relation string, targetIDs []string) error {
	if err := queries.RunMutation(ctx, x.querier,
		`mutation ($agent: String!, $src: String!, $rel: String!) {
			hub { db { agent {
				delete_skill_links(filter: {agent_id: {eq: $agent}, source_id: {eq: $src}, relation: {eq: $rel}}) { affected_rows }
			}}}
		}`,
		map[string]any{"agent": x.agentID, "src": sourceID, "rel": relation},
	); err != nil {
		return fmt.Errorf("skill: clear links %s/%s: %w", sourceID, relation, err)
	}
	for _, tid := range targetIDs {
		if tid == "" || tid == sourceID {
			continue // skip self-edges / unresolved members
		}
		if err := queries.RunMutation(ctx, x.querier,
			`mutation ($data: hub_db_skill_links_mut_input_data!) {
				hub { db { agent { insert_skill_links(data: $data) { source_id } } } }
			}`,
			map[string]any{"data": map[string]any{
				"agent_id":  x.agentID,
				"source_id": sourceID,
				"target_id": tid,
				"relation":  relation,
			}},
		); err != nil {
			return fmt.Errorf("skill: add link %s->%s/%s: %w", sourceID, tid, relation, err)
		}
	}
	return nil
}

// listMembers returns the skill rows reachable from sourceID along the
// given relation — the step-2 of two-step discovery (catalog →
// members). Resolves the `target` relation on skill_links in one query
// (the junction stays a plain table; the relation surfaces through it).
func (x *dynamicIndex) listMembers(ctx context.Context, sourceID, relation string) ([]skillRow, error) {
	type linkTarget struct {
		Target skillRow `json:"target"`
	}
	links, err := queries.RunQuery[[]linkTarget](ctx, x.querier,
		`query ($agent: String!, $src: String!, $rel: String!) {
			hub { db { agent {
				skill_links(filter: {agent_id: {eq: $agent}, source_id: {eq: $src}, relation: {eq: $rel}}) {
					target {`+skillRowProjection+`}
				}
			}}}
		}`,
		map[string]any{"agent": x.agentID, "src": sourceID, "rel": relation},
		"hub.db.agent.skill_links",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]skillRow, 0, len(links))
	for _, l := range links {
		if l.Target.ID != "" {
			out = append(out, l.Target)
		}
	}
	return out, nil
}

// --- manifest <-> row helpers ---

// catalogMemberNames derives a recipe catalog's member recipe names
// from its `allowed-tools` grants on the synthetic `task` provider —
// the 6.1d mechanism by which a `recipe_catalog` skill admits its
// recipes' `task:<name>` tools. Returns nil for non-catalog skills.
// De-duplicated; the literal `*` wildcard is skipped (it admits every
// task, not a named member).
func catalogMemberNames(m Manifest) []string {
	if !m.Hugen.RecipeCatalog {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	for _, g := range m.AllowedTools {
		if g.Provider != "task" {
			continue
		}
		for _, t := range g.Tools {
			if t == "" || t == "*" {
				continue
			}
			if _, ok := seen[t]; ok {
				continue
			}
			seen[t] = struct{}{}
			out = append(out, t)
		}
	}
	return out
}

// skillType classifies a manifest into the coarse `type` column used
// for the structural discovery prefilter.
func skillType(m Manifest) string {
	switch {
	case m.Hugen.RecipeCatalog:
		return "catalog"
	case m.Hugen.Task.Eligible:
		return "recipe"
	default:
		return "skill"
	}
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
	s := Skill{Manifest: m, Origin: OriginDynamic}
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

// bundleHash returns the sha256 of the bundle's SKILL.md — the
// update-at-start change signal. Cheap and sufficient for db-1; a
// whole-tree hash can replace it later without changing the column.
func bundleHash(dir string) string {
	content, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// --- dynamic backend (dir content + DB index) ---

// dynamicBackend is the Phase-6.2.db writable user source. Content
// lives on disk as bundles (the dirBackend write path, reused
// verbatim for atomicity); the DB index serves discovery. List reads
// the DB index (metadata only, no disk); Get reads the bundle from
// disk (full manifest incl. body) — the load path needs the prose.
// Publish writes both. Consolidates OriginLocal.
type dynamicBackend struct {
	dir   *dirBackend
	index *dynamicIndex
}

// newDynamicBackend wires the on-disk bundle root + the DB index.
func newDynamicBackend(root string, q types.Querier, agentID string, embedderEnabled bool) *dynamicBackend {
	return &dynamicBackend{
		dir:   &dirBackend{origin: OriginDynamic, root: root, writable: true},
		index: &dynamicIndex{querier: q, agentID: agentID, embedderEnabled: embedderEnabled},
	}
}

func (b *dynamicBackend) Origin() Origin { return OriginDynamic }

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
	if existing, err := b.index.getByName(ctx, sk.Manifest.Name); err == nil &&
		existing.ID != "" && hash != "" && existing.ContentHash == hash {
		return existing.ID, false, nil
	}
	id, err := b.index.upsert(ctx, sk.Manifest, source, dir, hash)
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

// relinkCatalogs rewrites every recipe-catalog's `catalog_member`
// edges from the index. Queries the catalog rows, derives each one's
// member recipe names from its manifest (metadata column), resolves
// names→ids against the index, and replaceLinks. Runs AFTER all
// bundles (hub + local) are indexed so member ids resolve regardless
// of which source dir a member lives in.
func (b *dynamicBackend) relinkCatalogs(ctx context.Context) error {
	cats, err := b.index.listCatalogs(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range cats {
		m, merr := manifestFromMetadata(c.Metadata)
		if merr != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.Name, merr))
			continue
		}
		var targetIDs []string
		for _, member := range catalogMemberNames(m) {
			if id, _ := b.index.getIDByName(ctx, member); id != "" {
				targetIDs = append(targetIDs, id)
			}
		}
		if err := b.index.replaceLinks(ctx, c.ID, "catalog_member", targetIDs); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.Name, err))
		}
	}
	return errors.Join(errs...)
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
	if _, err := b.index.upsert(ctx, m, "authored", dir, bundleHash(dir)); err != nil {
		return fmt.Errorf("skill: index after publish %q: %w", m.Name, err)
	}
	return nil
}

// Uninstall removes both the on-disk bundle and the index row. This
// is the only explicit removal path on the dynamic backend (bandit
// hygiene demotes but never deletes; reconcile only adds/updates).
func (b *dynamicBackend) Uninstall(ctx context.Context, name string) error {
	if err := b.index.deleteByName(ctx, name); err != nil {
		return fmt.Errorf("skill: uninstall index %q: %w", name, err)
	}
	dir := filepath.Join(b.dir.root, name)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("skill: uninstall bundle %q: %w", name, err)
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
// the DB at startup, then rewrites catalog_member edges. The relink
// runs over the whole index, so catalogs/members indexed from OTHER
// source dirs (hub, installed via IndexBundle before this call) are
// linked too. Returns the count of authored bundles indexed.
func (b *dynamicBackend) Reconcile(ctx context.Context) (int, error) {
	n, ierr := b.indexDir(ctx, b.dir.root, "authored")
	lerr := b.relinkCatalogs(ctx)
	return n, errors.Join(ierr, lerr)
}

// installFromDir indexes bundles from a read-only source dir (e.g. the
// materialised hub bundle dir) into the index under `source`, gated by
// the install set:
//
//   - declared == false  — install EVERY bundle in the dir (OOTB: the
//                           operator said nothing, ship the full set);
//   - declared == true   — install only the named bundles that exist
//                           in the dir (config is authoritative; an
//                           empty names list installs nothing).
//
// Does NOT relink catalogs — the caller runs relinkCatalogs once after
// all source dirs are indexed. Returns the count installed/updated.
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

	n := 0
	var errs []error
	for _, name := range want {
		if _, ok := onDisk[name]; !ok {
			continue // named in config but not present in the source dir
		}
		if err := ctx.Err(); err != nil {
			return n, err
		}
		if _, _, err := b.IndexBundle(ctx, filepath.Join(root, name), source); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// catalogMembersByName returns a dynamic catalog's member skills via
// the persisted catalog_member edges. Returns (nil, nil) when the
// catalog is not in the DB index or carries no edges — the caller
// then falls back to manifest-derived membership (covers hub catalogs
// not yet indexed into the dynamic store).
func (b *dynamicBackend) catalogMembersByName(ctx context.Context, name string) ([]Skill, error) {
	catID, err := b.index.getIDByName(ctx, name)
	if err != nil || catID == "" {
		return nil, err
	}
	rows, err := b.index.listMembers(ctx, catID, "catalog_member")
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	out := make([]Skill, 0, len(rows))
	for _, r := range rows {
		sk, rerr := rowToSkill(r)
		if rerr != nil {
			continue
		}
		out = append(out, sk)
	}
	return out, nil
}
