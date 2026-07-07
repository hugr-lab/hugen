package schema

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The data-object seed is small (one query filter + insert/update stamps per
// agent-scoped table); these tests pin it to the SDL so a new table cannot ship
// without an isolation rule, and guard the row shapes.

// sdlTables returns table name -> whether its body declares an agent_id column.
func sdlTables(t *testing.T) map[string]bool {
	t.Helper()
	re := regexp.MustCompile(`(?ms)^type (\w+) @table\(name: "\w+"\).*?^}`)
	body := regexp.MustCompile(`(?m)^\s+agent_id:`)
	tables := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?ms)^type (\w+) @table\(name: "(\w+)"\)(.*?)^}`).FindAllStringSubmatch(sdlTmpl, -1) {
		tables[m[1]] = body.MatchString(m[3])
	}
	if len(tables) == 0 {
		_ = re
		t.Fatal("no @table types parsed from schema.tmpl.graphql")
	}
	return tables
}

func TestAgentPermissions_CoversEveryAgentScopedTable(t *testing.T) {
	tables := sdlTables(t)
	for name, scoped := range tables {
		switch name {
		case "agents", "agent_types", "version":
			continue // identity/shared tables, seeded with dedicated rows
		}
		if !scoped {
			t.Errorf("table %s has no agent_id column — extend the seed with a dedicated data-object rule before shipping it", name)
		}
		if !slices.Contains(agentScopedTables, name) {
			t.Errorf("table %s missing from agentScopedTables — it would have no RLS floor", name)
		}
	}
	for _, name := range agentScopedTables {
		if _, ok := tables[name]; !ok {
			t.Errorf("agentScopedTables lists %s which is not in the SDL", name)
		}
	}
}

func TestAgentPermissions_RowSet(t *testing.T) {
	rows := AgentPermissions()
	seen := map[string]RolePermission{}
	for _, r := range rows {
		key := r.TypeName + "/" + r.FieldName
		if _, dup := seen[key]; dup {
			t.Errorf("duplicate permission row %s", key)
		}
		seen[key] = r
		// A row carries exactly one of {filter, data, disabled}.
		n := 0
		if r.Filter != nil {
			n++
		}
		if r.Data != nil {
			n++
		}
		if r.Disabled {
			n++
		}
		if n != 1 {
			t.Errorf("row %s must carry exactly one of filter/data/disabled, got %d", key, n)
		}
		if !strings.HasPrefix(r.FieldName, tablePrefix) {
			t.Errorf("row %s field_name must be a %s* GraphQL type name", key, tablePrefix)
		}
	}

	// Every agent-scoped table: query filter + insert/update stamp on agent_id.
	for _, tbl := range agentScopedTables {
		q, ok := seen[doQuery+"/"+tablePrefix+tbl]
		if !ok || q.Filter == nil {
			t.Errorf("%s: missing data-object:query filter", tbl)
		} else if _, hasAgent := q.Filter["agent_id"]; !hasAgent {
			t.Errorf("%s: query filter must scope agent_id, got %v", tbl, q.Filter)
		}
		for _, op := range []string{doInsert, doUpdate} {
			s, ok := seen[op+"/"+tablePrefix+tbl]
			if !ok || s.Data == nil {
				t.Errorf("%s: missing %s agent_id stamp", tbl, op)
			}
		}
	}

	// agents: scoped by PK; insert/delete denied.
	if a, ok := seen[doQuery+"/"+tablePrefix+"agents"]; !ok || a.Filter["id"] == nil {
		t.Errorf("agents must be data-object:query scoped on id (PK == agent), got %v", a.Filter)
	}
	for _, op := range []string{doInsert, doDelete} {
		if r, ok := seen[op+"/"+tablePrefix+"agents"]; !ok || !r.Disabled {
			t.Errorf("agents %s must be denied", op)
		}
	}
	// agent_types / version: mutations denied.
	for _, tbl := range []string{"agent_types", "version"} {
		for _, op := range []string{doInsert, doUpdate, doDelete} {
			if r, ok := seen[op+"/"+tablePrefix+tbl]; !ok || !r.Disabled {
				t.Errorf("%s %s must be denied", tbl, op)
			}
		}
	}
}

func TestAgentPermissions_LeavesArePlaceholders(t *testing.T) {
	// Our seed uses only [$auth.*] placeholder leaves; guard against a literal
	// sneaking in (the engine now supports richer filters, but the isolation
	// floor must stay principal-derived).
	var check func(t *testing.T, key string, v any)
	check = func(t *testing.T, key string, v any) {
		switch val := v.(type) {
		case map[string]any:
			for k, vv := range val {
				check(t, key+"."+k, vv)
			}
		case string:
			if !strings.HasPrefix(val, "[$auth.") {
				t.Errorf("%s: literal leaf %q — isolation floor must be [$auth.*]-derived", key, val)
			}
		default:
			t.Errorf("%s: non-string leaf %v (%T)", key, v, v)
		}
	}
	for _, r := range AgentPermissions() {
		if r.Filter != nil {
			check(t, r.TypeName+"/"+r.FieldName+" filter", r.Filter)
		}
		if r.Data != nil {
			check(t, r.TypeName+"/"+r.FieldName+" data", r.Data)
		}
	}
}
