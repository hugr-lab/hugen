package scheduler

import (
	"context"
	"fmt"
	"io/fs"
	"sync"
	"text/template"

	"github.com/hugr-lab/hugen/assets"
	"github.com/hugr-lab/hugen/pkg/extension"
	tplpkg "github.com/hugr-lab/hugen/pkg/runtime/template"
)

// cronSystemPromptPath is the embedded-FS path for the cron fire
// system prompt template, scoped to assets/prompts (Renderer-style)
// without the `.tmpl` suffix. The scheduler reads it via
// [assets.PromptsFS] directly rather than going through the shared
// [prompts.Renderer]: the cron prompt needs the `formatDate` /
// `addDuration` funcmap entries from [tplpkg.FuncMap] which the
// Renderer's text/template namespace doesn't expose.
const cronSystemPromptPath = "prompts/task/cron_system.tmpl"

// AdvertiseSystemPrompt implements [extension.Advertiser]. Renders
// the cron-contract block on sessions firing under a schedule (the
// FireContext stamp is present). Non-fire sessions get the empty
// string; recipe discovery is the task ext's responsibility — task-
// eligible recipes surface as synthetic `task:<recipe>` tools, not
// prose blocks.
//
// Idempotent: the cron-contract template is parsed once and cached
// on first render.
func (e *Extension) AdvertiseSystemPrompt(_ context.Context, state extension.SessionState) string {
	fc, ok := fireContextFromState(state)
	if !ok || fc == nil {
		return ""
	}
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
