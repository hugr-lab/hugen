package scheduler

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"text/template"

	"github.com/hugr-lab/hugen/assets"
	"github.com/hugr-lab/hugen/pkg/extension"
	tplpkg "github.com/hugr-lab/hugen/pkg/runtime/template"
	"github.com/hugr-lab/hugen/pkg/skill"
)

// cronSystemPromptPath is the embedded-FS path for the cron fire
// system prompt template, scoped to assets/prompts (Renderer-style)
// without the `.tmpl` suffix. The scheduler reads it via
// [assets.PromptsFS] directly rather than going through the shared
// [prompts.Renderer]: the cron prompt needs the `formatDate` /
// `addDuration` funcmap entries from [tplpkg.FuncMap] which the
// Renderer's text/template namespace doesn't expose.
const cronSystemPromptPath = "prompts/task/cron_system.tmpl"

// AdvertiseSystemPrompt implements [extension.Advertiser]. Two
// mutually exclusive surfaces share this hook:
//
//   - Root sessions (depth 0) get the `## Available tasks` catalog
//     listing every skill flagged `metadata.hugen.task.eligible:
//     true`. The root reads this to discover which recipes it can
//     run ad-hoc (via `_run_task` mission) or schedule (via
//     `task:create`). Mirrors mission ext's "Available missions"
//     pattern (pkg/extension/mission/dispatcher.go). Phase 6.1d.
//   - Cron-fire sessions get the bundled cron-contract block. Phase
//     6 §1.2.4 / §3.5.
//
// Non-root sessions without a FireContext get the empty string
// (skipped by the runtime's concatenator).
//
// Idempotent: the cron-contract template is parsed once and cached
// on first render; the tasks block re-enumerates the skill manager
// each call so freshly published task-eligible skills surface
// without a session restart.
func (e *Extension) AdvertiseSystemPrompt(ctx context.Context, state extension.SessionState) string {
	// FireContext wins as the most specific signal: a session that
	// carries one is a cron fire by construction and gets the
	// contract block regardless of nominal depth. Production cron
	// fires sit at depth > 0 (subagent of owner root); tests stamp
	// the same key on a fake root and expect contract rendering.
	if fc, ok := fireContextFromState(state); ok && fc != nil {
		tmpl, err := e.cronPromptTemplate()
		if err != nil {
			e.logger.Warn("scheduler: cron prompt parse",
				"session", state.SessionID(), "err", err)
			return ""
		}
		rendered, err := tplpkg.RenderInto(tmpl, tplpkg.NewFireRenderContext(fc))
		if err != nil {
			e.logger.Warn("scheduler: cron prompt render",
				"session", state.SessionID(), "task_id", fc.TaskID, "err", err)
			return ""
		}
		return rendered
	}
	if state != nil && state.Depth() == 0 {
		return e.renderAvailableTasks(ctx, state)
	}
	return ""
}

// renderAvailableTasks returns the `## Available tasks` block. Empty
// when the skill manager carries no task-eligible skills (the
// catalogue is opt-in — a fresh agent without recipes contributes
// nothing).
func (e *Extension) renderAvailableTasks(ctx context.Context, state extension.SessionState) string {
	if e.skills == nil {
		return ""
	}
	all, err := e.skills.List(ctx)
	if err != nil {
		e.logger.Warn("scheduler: list skills for tasks catalogue",
			"session", state.SessionID(), "err", err)
		return ""
	}
	entries := taskEligibleEntries(all)
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Available tasks\n\n")
	b.WriteString("Recipes that can be RUN. Two paths:\n")
	b.WriteString("- Ad-hoc: `session:spawn_mission(skill=\"_run_task\", inputs={task_skill, ...})`\n")
	b.WriteString("- Scheduled: `task:create(skill_ref=<name>, schedule_spec, inputs)`\n\n")
	for _, entry := range entries {
		fmt.Fprintf(&b, "- `%s`", entry.Name)
		if entry.Kind != "" {
			fmt.Fprintf(&b, " (kind=%s)", entry.Kind)
		}
		if entry.Summary != "" {
			fmt.Fprintf(&b, " — %s", entry.Summary)
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// TaskCatalogEntry is one row in the Available tasks block. Kept
// exported so external callers (TUI, /tasks slash command status,
// future hub APIs) can render the same catalogue without re-deriving
// the eligibility predicate.
type TaskCatalogEntry struct {
	Name    string
	Kind    string
	Summary string
}

// taskEligibleEntries filters and sorts task-eligible skills for the
// catalogue. Sort is alphabetical by name so the LLM sees a stable
// ordering across renders.
func taskEligibleEntries(all []skill.Skill) []TaskCatalogEntry {
	out := make([]TaskCatalogEntry, 0, len(all))
	for _, sk := range all {
		if !sk.Manifest.Hugen.Task.Eligible {
			continue
		}
		summary := strings.TrimSpace(sk.Manifest.Hugen.Task.GoalSummary)
		if summary == "" {
			summary = strings.TrimSpace(sk.Manifest.Description)
		}
		out = append(out, TaskCatalogEntry{
			Name:    sk.Manifest.Name,
			Kind:    strings.TrimSpace(sk.Manifest.Hugen.Task.Kind),
			Summary: summary,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// cronPromptCache lives at package scope rather than on *Extension
// because the template is process-wide content; one parsed tree
// serves every cron fire across every session. The sync.Once
// ensures concurrent fires never race on parse.
var (
	cronPromptOnce sync.Once
	cronPromptTpl  *template.Template
	cronPromptErr  error
)

// cronPromptTemplate loads + caches the parsed cron-contract
// template on first call. Read errors propagate to AdvertiseSystemPrompt
// which logs and degrades — a missing template should be loud at
// boot (the binary's embed manifest is the source of truth) but
// must not crash the fire dispatch.
func (e *Extension) cronPromptTemplate() (*template.Template, error) {
	cronPromptOnce.Do(func() {
		body, err := fs.ReadFile(assets.PromptsFS, cronSystemPromptPath)
		if err != nil {
			cronPromptErr = fmt.Errorf("read cron prompt %s: %w", cronSystemPromptPath, err)
			return
		}
		t, err := template.New("scheduler.cron_system").Funcs(tplpkg.FuncMap()).Parse(string(body))
		if err != nil {
			cronPromptErr = fmt.Errorf("parse cron prompt: %w", err)
			return
		}
		cronPromptTpl = t
	})
	return cronPromptTpl, cronPromptErr
}

// resetCronPromptCacheForTest clears the package-level cache. Test
// hook only — production callers never invoke this.
//
//nolint:unused // referenced from *_test.go via package scope
func resetCronPromptCacheForTest() {
	cronPromptOnce = sync.Once{}
	cronPromptTpl = nil
	cronPromptErr = nil
}

// Compile-time assertion: the extension contributes a system-prompt
// block. Anchored here next to the implementation so a future
// signature drift fails at scheduler-package compile time.
var _ extension.Advertiser = (*Extension)(nil)
