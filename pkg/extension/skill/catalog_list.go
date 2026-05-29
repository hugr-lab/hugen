package skill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	skillpkg "github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// `skill:catalog_list` is the skill-inventory surface — distinct from
// `skill:tools_catalog`, which is keyed on registered TOOLS. This one
// is keyed on SKILLS: it answers "what reusable skills already exist
// in the store?", the question `_task_builder`'s researcher and root's
// schedule-intent flow ask before composing a task from scratch. A
// task IS a standalone skill (a task-eligible recipe bundle), so a
// catalogue match means the work can be scheduled directly instead of
// rebuilt.
//
// Unlike `tools_catalog`, this surface does NOT apply the caller's
// tier filter. Listing is pure read-only inventory — nothing is
// loaded as a side effect — and a task-eligible recipe runs in its
// own (typically worker-tier) recipe / cron session, not in the
// caller's. A mission-tier researcher browsing for a matching task
// must still see worker-tier recipes; tier-filtering here would hide
// the very rows the discovery is for.

const (
	toolNameCatalogList   = providerName + ":catalog_list"
	permObjectCatalogList = "hugen:tool:system"
	toolDescCatalogList   = "Search the agent's skill catalogue — every saved / bundled skill, with its description and (for task-eligible skills) goal_summary, kind, and whether it declares an inputs_schema. Use this BEFORE composing a task from scratch: a matching task-eligible skill can be run via `task:<name>` or scheduled via `schedule:create` directly, with no rebuild. Two-step discovery: search with `type=catalog` to find recipe collections, then pass `within=<catalog>` to list the recipes inside one. With a `keyword` the catalogue is ranked by relevance (semantic when available, substring otherwise) and capped at `limit`. Optional filters: `task_eligible` (bool), `type` (catalog / recipe / skill), `keyword`, `within`, `limit`."
	catalogListSchema     = `{
  "type": "object",
  "properties": {
    "task_eligible": {"type": "boolean", "description": "When true, return only task-eligible skills (those runnable via task:<name> and schedulable). Omit to list every skill."},
    "keyword":       {"type": "string", "description": "Free-text query. Ranked by semantic relevance when an embedder is available, else a case-insensitive substring match against name, description, and keywords."},
    "type":          {"type": "string", "description": "Optional coarse filter on the skill class: 'catalog' (recipe collections), 'recipe' (task-runnable), or 'skill'."},
    "within":        {"type": "string", "description": "Name of a recipe catalog: returns the recipes that belong to it (two-step discovery, step 2). Other filters apply to the member set."},
    "limit":         {"type": "integer", "description": "Max results to return when a keyword is given (default 10). Ignored for an unfiltered listing."}
  }
}`
)

type catalogListInput struct {
	TaskEligible *bool  `json:"task_eligible,omitempty"`
	Keyword      string `json:"keyword,omitempty"`
	Type         string `json:"type,omitempty"`
	Within       string `json:"within,omitempty"`
	Limit        int    `json:"limit,omitempty"`
}

type catalogListEntry struct {
	Name            string   `json:"name"`
	Description     string   `json:"description,omitempty"`
	TaskEligible    bool     `json:"task_eligible"`
	TaskKind        string   `json:"task_kind,omitempty"`
	GoalSummary     string   `json:"goal_summary,omitempty"`
	Keywords        []string `json:"keywords,omitempty"`
	HasInputsSchema bool     `json:"has_inputs_schema,omitempty"`
}

type catalogListResult struct {
	Skills []catalogListEntry `json:"skills"`
}

func (h *SessionSkill) callCatalogList(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if h.manager == nil {
		return nil, tool.ErrSystemUnavailable
	}
	var in catalogListInput
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("%w: skill:catalog_list: %v", tool.ErrArgValidation, err)
		}
	}
	query := strings.TrimSpace(in.Keyword)

	// Two-step discovery, step 2: expand a named recipe catalog into
	// its member recipes. Other filters (task_eligible / type /
	// keyword) narrow the member set.
	if within := strings.TrimSpace(in.Within); within != "" {
		members, err := h.manager.CatalogMembers(ctx, within)
		if err != nil {
			if errors.Is(err, skillpkg.ErrSkillNotFound) {
				return json.Marshal(catalogListResult{Skills: []catalogListEntry{}})
			}
			return nil, fmt.Errorf("skill:catalog_list: catalog members of %q: %w", within, err)
		}
		kw := strings.ToLower(query)
		out := catalogListResult{Skills: make([]catalogListEntry, 0, len(members))}
		for _, sk := range members {
			m := sk.Manifest
			if in.TaskEligible != nil && *in.TaskEligible && !m.Hugen.Task.Eligible {
				continue
			}
			if in.Type != "" && !catalogMatchesType(in.Type, m) {
				continue
			}
			if kw != "" && !catalogMatchesKeyword(kw, m.Name, m.Description, m.Hugen.Mission.Keywords) {
				continue
			}
			out.Skills = append(out.Skills, catalogEntryFromSkill(sk))
		}
		sort.Slice(out.Skills, func(i, j int) bool { return out.Skills[i].Name < out.Skills[j].Name })
		return json.Marshal(out)
	}

	// PRIMARY path: when a query is given, try semantic discovery over
	// the DB index (Phase 6.2.db). Results come back ranked + capped;
	// preserve that order (no re-sort). ErrNoEmbedder (no dynamic
	// backend / no embedder) degrades to the substring scan below —
	// the stable seam the catalogue evolved from.
	if query != "" {
		ranked, err := h.manager.Search(ctx, query, skillpkg.SearchOpts{
			TaskEligible: in.TaskEligible,
			Type:         in.Type,
			Limit:        in.Limit,
		})
		switch {
		case err == nil:
			out := catalogListResult{Skills: make([]catalogListEntry, 0, len(ranked))}
			for _, sk := range ranked {
				out.Skills = append(out.Skills, catalogEntryFromSkill(sk))
			}
			return json.Marshal(out)
		case errors.Is(err, skillpkg.ErrNoEmbedder):
			// fall through to the keyword/substring scan
		default:
			return nil, fmt.Errorf("skill:catalog_list: search: %w", err)
		}
	}

	keyword := strings.ToLower(query)
	all, err := h.manager.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("skill:catalog_list: list skills: %w", err)
	}

	out := catalogListResult{Skills: make([]catalogListEntry, 0, len(all))}
	for _, sk := range all {
		m := sk.Manifest
		tb := m.Hugen.Task
		if in.TaskEligible != nil && *in.TaskEligible && !tb.Eligible {
			continue
		}
		if in.Type != "" && !catalogMatchesType(in.Type, m) {
			continue
		}
		keywords := m.Hugen.Mission.Keywords
		if keyword != "" && !catalogMatchesKeyword(keyword, m.Name, m.Description, keywords) {
			continue
		}
		out.Skills = append(out.Skills, catalogEntryFromSkill(sk))
	}
	sort.Slice(out.Skills, func(i, j int) bool { return out.Skills[i].Name < out.Skills[j].Name })
	return json.Marshal(out)
}

// catalogEntryFromSkill projects a Skill into the model-facing
// catalogue entry. Shared by the semantic-ranked and substring paths
// so both surface the same shape.
func catalogEntryFromSkill(sk skillpkg.Skill) catalogListEntry {
	m := sk.Manifest
	tb := m.Hugen.Task
	entry := catalogListEntry{
		Name:            m.Name,
		Description:     strings.TrimSpace(m.Description),
		TaskEligible:    tb.Eligible,
		Keywords:        m.Hugen.Mission.Keywords,
		HasInputsSchema: len(tb.InputsSchema) > 0,
	}
	if tb.Eligible {
		entry.TaskKind = tb.Kind
		entry.GoalSummary = strings.TrimSpace(tb.GoalSummary)
	}
	return entry
}

// catalogMatchesType reports whether the manifest's coarse class
// matches the requested `type` filter (catalog / recipe / skill).
// Mirrors the DB `type` column's skillType classifier so the
// substring fallback agrees with the semantic path.
func catalogMatchesType(typ string, m skillpkg.Manifest) bool {
	switch typ {
	case "catalog":
		return m.Hugen.RecipeCatalog
	case "recipe":
		return m.Hugen.Task.Eligible && !m.Hugen.RecipeCatalog
	case "skill":
		return !m.Hugen.Task.Eligible && !m.Hugen.RecipeCatalog
	default:
		return true
	}
}

// catalogMatchesKeyword reports whether the lower-cased keyword is a
// substring of the skill name, description, or any keyword. Keyword
// is already lower-cased + trimmed by the caller.
func catalogMatchesKeyword(keyword, name, desc string, keywords []string) bool {
	if strings.Contains(strings.ToLower(name), keyword) ||
		strings.Contains(strings.ToLower(desc), keyword) {
		return true
	}
	for _, kw := range keywords {
		if strings.Contains(strings.ToLower(kw), keyword) {
			return true
		}
	}
	return false
}
