package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hugr-lab/hugen/pkg/skill"
)

// searchTaskEntry is one hit from `task:search` — just enough to pick a
// task by intent. The caller follows up with `task:describe(name)` for
// the input contract and `task:execute_task` to run it.
type searchTaskEntry struct {
	Name            string `json:"name"`
	Description     string `json:"description,omitempty"`
	HasInputsSchema bool   `json:"has_inputs_schema,omitempty"`
}

type searchTaskResult struct {
	Tasks []searchTaskEntry `json:"tasks"`
}

// callSearch implements `task:search(query)` — the explicit-query
// task-discovery surface, the active complement to the passive
// `## Available tasks` advertise (which is ranked + capped, so absence
// there proves nothing). It searches ONLY task-eligible skills (built
// tasks), so it is the task analogue of `skill:catalog_list` for skills —
// the two never overlap. Semantic when an embedder is wired, substring
// otherwise. Pure read — no spawn, no approval.
func (e *Extension) callSearch(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	var in struct {
		Query string `json:"query"`
		Limit int    `json:"limit,omitempty"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return toolErr("invalid_args",
				fmt.Sprintf("task:search args is not valid JSON: %v", err)), nil
		}
	}
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return toolErr("invalid_args", "task:search requires a non-empty query"), nil
	}
	if e.skills == nil {
		return toolErr("no_skill_manager", "skill manager not wired"), nil
	}

	// PRIMARY: semantic search scoped to task-eligible skills. Degrades to
	// the substring scan on ErrNoEmbedder (no dynamic backend / embedder).
	taskOnly := true
	ranked, err := e.skills.Search(ctx, query, skill.SearchOpts{TaskEligible: &taskOnly, Limit: in.Limit})
	switch {
	case err == nil:
		out := searchTaskResult{Tasks: make([]searchTaskEntry, 0, len(ranked))}
		for _, sk := range ranked {
			out.Tasks = append(out.Tasks, searchEntryFromSkill(sk))
		}
		return json.Marshal(out)
	case errors.Is(err, skill.ErrNoEmbedder):
		// fall through to the substring scan
	default:
		return nil, fmt.Errorf("task:search: search %q: %w", query, err)
	}

	all, lerr := e.skills.List(ctx)
	if lerr != nil {
		return nil, fmt.Errorf("task:search: list skills: %w", lerr)
	}
	kw := strings.ToLower(query)
	out := searchTaskResult{Tasks: make([]searchTaskEntry, 0, len(all))}
	for _, sk := range all {
		tb := sk.Manifest.Hugen.Task
		if !tb.Eligible {
			continue
		}
		if !searchMatchesKeyword(kw, sk.Manifest.Name, sk.Manifest.Description, tb.GoalSummary) {
			continue
		}
		out.Tasks = append(out.Tasks, searchEntryFromSkill(sk))
	}
	sort.Slice(out.Tasks, func(i, j int) bool { return out.Tasks[i].Name < out.Tasks[j].Name })
	return json.Marshal(out)
}

// searchEntryFromSkill projects a task-eligible skill into a search hit.
// Description prefers the task's imperative goal_summary, falling back to
// the manifest description.
func searchEntryFromSkill(sk skill.Skill) searchTaskEntry {
	tb := sk.Manifest.Hugen.Task
	desc := strings.TrimSpace(tb.GoalSummary)
	if desc == "" {
		desc = strings.TrimSpace(sk.Manifest.Description)
	}
	return searchTaskEntry{
		Name:            sk.Manifest.Name,
		Description:     desc,
		HasInputsSchema: len(tb.InputsSchema) > 0,
	}
}

// searchMatchesKeyword reports whether the lower-cased keyword is a
// substring of the task's name, description, or goal_summary. The
// fallback path when no embedder is wired.
func searchMatchesKeyword(keyword, name, desc, goalSummary string) bool {
	return strings.Contains(strings.ToLower(name), keyword) ||
		strings.Contains(strings.ToLower(desc), keyword) ||
		strings.Contains(strings.ToLower(goalSummary), keyword)
}
