// Package task hosts the synthetic recipe-runner surface introduced
// by Phase 6.1d. It owns the `task:*` tool namespace exposed to the
// model and dispatches calls into a fresh subagent (or full mission)
// per recipe-skill manifest.
//
// Where it sits in the architecture:
//
//   - Scheduler ext (`pkg/extension/scheduler`) owns `schedule:*`:
//     create / list / pause / resume / cancel. That surface is for
//     binding a schedule to a recipe; the runner fires the recipe
//     per-tick under the owner root.
//   - Task ext (this package) owns `task:*`: one synthetic tool per
//     task-eligible skill, each parameterised by the manifest's
//     `task.inputs_schema`. The model calls `task:<recipe>(args)` to
//     run a recipe immediately, without persistence.
//
// Both extensions read recipe metadata from the same
// `pkg/skill.SkillManager`. Their tool namespaces are intentionally
// distinct so the same recipe can serve both ad-hoc execution
// (`task:<recipe>`) and scheduled execution (`schedule:create
// skill_ref=<recipe>`) with no name collision.
package task

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

const providerName = "task"

// PermRunRecipe is the umbrella permission object every synthetic
// `task:<recipe-name>` tool advertises. Per-recipe policy refinement
// lands later (see backlog 5.3.policy-ux); today the loaded category
// skill's `allowed-tools` list is the gate that decides which
// recipe tools admit into a session's catalogue.
const PermRunRecipe = "hugen:task:run_recipe"

// PermExecuteTask gates the generic `task:execute_task` tool — the
// discovery-then-run path that runs ANY task-eligible skill by name
// (vs. the per-recipe `task:<name>` tools gated by PermRunRecipe). A
// distinct permission object so policies / allow-sets can grant the
// generic runner without admitting every synthetic recipe tool.
const PermExecuteTask = "hugen:task:execute_task"

// toolNameExecuteTask is the bare (provider-stripped) name of the
// generic runner. Call intercepts it BEFORE the synthetic-recipe
// lookup so it never collides with a `task:<recipe>` skill name.
const toolNameExecuteTask = "execute_task"

// SessionHost is the narrow surface the dispatch path needs from the
// runtime's session supervisor. Mirrors the scheduler ext's
// SessionHost — the two could share a common interface in a future
// refactor, but living separately keeps each extension self-contained
// (no cross-extension imports). Production binding goes through
// pkg/runtime; tests inject a fake.
type SessionHost interface {
	// Get looks up a live session by id. Used by the dispatch path
	// to find the caller's root session before spawning the recipe
	// subagent under it.
	Get(id string) (*session.Session, bool)
}

// Extension owns the synthetic `task:*` tool surface. The
// SkillManager reference is the source of truth for which manifests
// contribute synthetic tools; tools are derived per-call from the
// live skill catalogue so newly-published recipes surface without a
// session restart.
type Extension struct {
	skills  *skill.SkillManager
	logger  *slog.Logger
	agentID string // stamped onto the synthetic first-message participant the dispatch path injects into the recipe child.

	mu   sync.RWMutex
	host SessionHost

	// spawnCounter generates monotonic unique tokens for ad-hoc
	// spawn names so concurrent recipe invocations under the same
	// caller can't collide on subagent names.
	spawnCounter atomic.Int64
}

// NewExtension constructs the task extension. SkillManager may be
// nil in pathological test setups — the resulting List returns an
// empty surface, and Call rejects every name. agentID is stamped
// onto the synthetic UserMessage the dispatch path injects into
// the recipe child to drive its first turn — see dispatch.go.
func NewExtension(skills *skill.SkillManager, agentID string, logger *slog.Logger) *Extension {
	if logger == nil {
		logger = slog.Default()
	}
	return &Extension{
		skills:  skills,
		agentID: agentID,
		logger:  logger,
	}
}

// Bind installs the SessionHost the dispatch path needs. Called
// once at boot AFTER the runtime's session manager exists (see
// pkg/runtime wiring). Calling Bind with nil leaves the existing
// reference in place.
func (e *Extension) Bind(host SessionHost) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if host != nil {
		e.host = host
	}
}

func (e *Extension) sessionHost() SessionHost {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.host
}

// Compile-time interface assertions.
var (
	_ extension.Extension = (*Extension)(nil)
	_ tool.ToolProvider   = (*Extension)(nil)
)

// Name implements [extension.Extension] and [tool.ToolProvider].
func (e *Extension) Name() string { return providerName }

// Lifetime implements [tool.ToolProvider]. The provider is process-
// wide stateful (SkillManager + SessionHost) so per-agent lifetime
// matches scheduler ext's choice — one instance per agent process.
func (e *Extension) Lifetime() tool.Lifetime { return tool.LifetimePerAgent }

// List implements [tool.ToolProvider]. Emits one synthetic
// `task:<recipe-name>` tool per task-eligible skill in the manager.
// FilterTools (skill ext) narrows the catalogue per session against
// the loaded category-skills' allow-set, so root sees only what its
// loaded categories admit.
//
// Tool fields:
//
//   - Name: `task:<skill-name>`
//   - Description: `task.goal_summary`, falls back to
//     `manifest.description`. Always trimmed.
//   - ArgSchema: `task.inputs_schema` marshalled verbatim. A
//     permissive default `{type:object, additionalProperties: true}`
//     is supplied when the recipe declares no schema, so the model
//     still sees a valid JSON Schema shape.
//   - PermissionObject: [PermRunRecipe] (shared umbrella).
func (e *Extension) List(ctx context.Context) ([]tool.Tool, error) {
	if e.skills == nil {
		return nil, nil
	}
	all, err := e.skills.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("task: list skills: %w", err)
	}
	out := make([]tool.Tool, 0, len(all))
	for _, sk := range all {
		tb := sk.Manifest.Hugen.Task
		if !tb.Eligible {
			continue
		}
		desc := strings.TrimSpace(tb.GoalSummary)
		if desc == "" {
			desc = strings.TrimSpace(sk.Manifest.Description)
		}
		var argSchema json.RawMessage
		if len(tb.InputsSchema) > 0 {
			raw, mErr := json.Marshal(tb.InputsSchema)
			if mErr != nil {
				return nil, fmt.Errorf("task: marshal inputs_schema for %q: %w",
					sk.Manifest.Name, mErr)
			}
			argSchema = raw
		} else {
			argSchema = json.RawMessage(`{"type":"object","properties":{},"additionalProperties":true}`)
		}
		out = append(out, tool.Tool{
			Name:             providerName + ":" + sk.Manifest.Name,
			Description:      desc,
			Provider:         providerName,
			PermissionObject: PermRunRecipe,
			ArgSchema:        argSchema,
		})
	}
	// The generic discovery-then-run tool: always present (one tool, not
	// per-recipe) so a caller that found a task by search / catalog can
	// run it by name without the per-recipe synthetic tool being
	// surfaced. Allow-set gated like any tool — see _root / mission-role
	// grants.
	out = append(out, executeTaskTool())
	return out, nil
}

// executeTaskTool returns the static `task:execute_task` descriptor.
func executeTaskTool() tool.Tool {
	return tool.Tool{
		Name:             providerName + ":" + toolNameExecuteTask,
		Description:      descExecuteTask,
		Provider:         providerName,
		PermissionObject: PermExecuteTask,
		ArgSchema:        json.RawMessage(schemaExecuteTask),
	}
}

const descExecuteTask = "Run a built task (a task-eligible skill) by name with inputs — the discovery-then-run path. Find the task by intent with `skill:catalog_list(task_eligible:true)`, collect any required inputs from the user, then run it here. It runs as a worker and the result lands in this conversation. Prefer this over spawning a mission to do work a built task already covers — never re-implement from scratch a task that already exists."

const schemaExecuteTask = `{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "The task-eligible skill name to run (from skill:catalog_list with task_eligible:true)."},
    "inputs": {"type": "object", "description": "Input values for the task's declared inputs_schema. Omit for a no-input task.", "additionalProperties": true}
  },
  "required": ["name"]
}`

// Subscribe implements [tool.ToolProvider]. The synthetic surface
// re-derives per List call from the skill manager, so a static
// nil-channel subscription is sufficient — refresh events propagate
// naturally through the skill ext's existing generation bumps.
func (e *Extension) Subscribe(_ context.Context) (<-chan tool.ProviderEvent, error) {
	return nil, nil
}

// Close implements [tool.ToolProvider]. Nothing to release — the
// SkillManager + SessionHost are owned by the runtime.
func (e *Extension) Close() error { return nil }

// stripProviderPrefix returns the recipe-name portion of a fully-
// qualified tool name. Returns the input unchanged when the prefix
// is absent — the caller then errors out via the unknown-tool path.
func stripProviderPrefix(name string) string {
	pfx := providerName + ":"
	if strings.HasPrefix(name, pfx) {
		return name[len(pfx):]
	}
	return name
}
