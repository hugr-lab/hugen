package schema

// RLS permission seed for the agent roles (design 008 spec-hub-side §5 HB3),
// built on query-engine's **data-object (table-level) permissions** — a filter
// keyed on the table's GraphQL type name that composes on EVERY path the table
// is reached (direct list, _by_pk, forward/reverse relations, _join,
// aggregations, and — for insert/update — force-stamped mutation data). See
// `../hugr-lab.github.io/docs/.../access-control#data-object-table-level-permissions`
// and `design/008-integration/design-data-object-permissions.md`.
//
// The schema library ships the row set for its OWN tables; the hub adds
// platform-surface deny rows and applies both to `core.role_permissions` for
// the roles `agent` and `agent_template`.
//
// Row shape (all in the `permissions` table via synthetic type_name):
//   - data-object:query   filter → every read + the update/delete WHERE fallback
//   - data-object:insert  data   → force-stamped on insert (incl. nested)
//   - data-object:update  data   → force-stamped on update SET
//   - `disabled: true` on data-object:query denies the table on every path;
//     on an operation row it denies just that operation.
//
// `field_name` is the data-object's GraphQL TYPE NAME — source-prefixed
// `hub_agent_db_<table>` (the agent store registers under source `hub.agent.db`
// in both local and hub mode → prefix `hub_agent_db`).
//
// Isolation floor: the agent's hugr principal is provisioned with
// user_id == agent_id (D8), so `agent_id == [$auth.user_id]` scopes each table
// to its owning agent. `data-object:insert`/`update` force-stamp agent_id so a
// cross-agent write is impossible at the engine — the explicit hugen-side
// filter becomes defence-in-depth, no longer the sole floor.

// RolePermission is one `core.role_permissions` row, minus the role name — the
// hub applier stamps the same set onto every agent role it seeds.
type RolePermission struct {
	TypeName  string         `json:"type_name"`
	FieldName string         `json:"field_name"`
	Filter    map[string]any `json:"filter,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
	Disabled  bool           `json:"disabled"`
}

// Data-object permission type_names (query-engine access-control).
const (
	doQuery  = "data-object:query"
	doInsert = "data-object:insert"
	doUpdate = "data-object:update"
	doDelete = "data-object:delete"

	// tablePrefix is the source-derived GraphQL type prefix for the agent store
	// (source path `hub.agent.db` → prefix `hub_agent_db`).
	tablePrefix = "hub_agent_db_"
)

// agentScopedTables carry an agent_id column and are scoped to the owning agent.
var agentScopedTables = []string{
	"sessions",
	"session_events",
	"session_notes",
	"tool_policies",
	"tasks",
	"task_log",
	"skills",
	"skill_log",
	"skill_links",
}

func agentIDFilter() map[string]any { return map[string]any{"agent_id": map[string]any{"eq": authUserID}} }
func agentIDData() map[string]any   { return map[string]any{"agent_id": authUserID} }

// selfIDFilter scopes the agents table itself: its PK IS the agent id.
func selfIDFilter() map[string]any { return map[string]any{"id": map[string]any{"eq": authUserID}} }

const authUserID = "[$auth.user_id]"

// AgentPermissions returns the agent-store data-object permission rows an
// agent-tier role must carry. The hub applies these (plus its platform deny
// rows) to `agent` and `agent_template` at boot.
func AgentPermissions() []RolePermission {
	var rows []RolePermission
	query := func(t string, filter map[string]any) {
		rows = append(rows, RolePermission{TypeName: doQuery, FieldName: tablePrefix + t, Filter: filter})
	}
	stamp := func(t string, op string, data map[string]any) {
		rows = append(rows, RolePermission{TypeName: op, FieldName: tablePrefix + t, Data: data})
	}
	deny := func(t string, op string) {
		rows = append(rows, RolePermission{TypeName: op, FieldName: tablePrefix + t, Disabled: true})
	}

	// Agent-scoped tables: read-floor by agent_id (composes to every path);
	// insert/update force-stamp agent_id so writes cannot cross agents.
	for _, t := range agentScopedTables {
		query(t, agentIDFilter())
		stamp(t, doInsert, agentIDData())
		stamp(t, doUpdate, agentIDData())
	}

	// agents: readable ONLY as the own row (PK == principal). Identity rows are
	// provisioned by the hub under its own principal, never by the agent — so
	// insert/update/delete are all denied. Update is security-critical: the `role`
	// column stamps the agent's JWT, and data-object perms are table-level (can't
	// spare just `role`), so a self-update would let an agent set role=admin and
	// self-escalate past this whole floor on its next token. hugen never issues
	// update_agents; the hub owns all agents writes under its admin principal.
	query("agents", selfIDFilter())
	deny("agents", doInsert)
	deny("agents", doUpdate)
	deny("agents", doDelete)

	// agent_types (shared template) + version (infra pins): readable by default,
	// immutable for agents.
	for _, t := range []string{"agent_types", "version"} {
		deny(t, doInsert)
		deny(t, doUpdate)
		deny(t, doDelete)
	}

	return rows
}
