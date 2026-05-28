package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

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
	toolDescCatalogList   = "Browse the agent's skill catalogue — every saved / bundled skill, with its description and (for task-eligible skills) goal_summary, kind, and whether it declares an inputs_schema. Use this BEFORE composing a task from scratch: a matching task-eligible skill can be run via `task:<name>` or scheduled via `schedule:create` directly, with no rebuild. Optional filters: `task_eligible` (bool — only task-runnable skills) + `keyword` (case-insensitive substring matched against name, description, and keywords)."
	catalogListSchema     = `{
  "type": "object",
  "properties": {
    "task_eligible": {"type": "boolean", "description": "When true, return only task-eligible skills (those runnable via task:<name> and schedulable). Omit to list every skill."},
    "keyword":       {"type": "string", "description": "Optional case-insensitive substring matched against the skill name, description, and keywords."}
  }
}`
)

type catalogListInput struct {
	TaskEligible *bool  `json:"task_eligible,omitempty"`
	Keyword      string `json:"keyword,omitempty"`
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
	keyword := strings.ToLower(strings.TrimSpace(in.Keyword))

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
		keywords := m.Hugen.Mission.Keywords
		if keyword != "" && !catalogMatchesKeyword(keyword, m.Name, m.Description, keywords) {
			continue
		}
		entry := catalogListEntry{
			Name:            m.Name,
			Description:     strings.TrimSpace(m.Description),
			TaskEligible:    tb.Eligible,
			Keywords:        keywords,
			HasInputsSchema: len(tb.InputsSchema) > 0,
		}
		if tb.Eligible {
			entry.TaskKind = tb.Kind
			entry.GoalSummary = strings.TrimSpace(tb.GoalSummary)
		}
		out.Skills = append(out.Skills, entry)
	}
	sort.Slice(out.Skills, func(i, j int) bool { return out.Skills[i].Name < out.Skills[j].Name })
	return json.Marshal(out)
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
