package skill

import (
	"errors"
	"fmt"
	"strings"

	"github.com/oasdiff/yaml"
)

// ErrTaskBlockMisplaced is returned by [ValidateTaskAuthoring] when a
// manifest carries a task signal in the wrong place — the dominant
// run-2 dogfood failure: the authoring model wrote the `task` block
// at the TOP LEVEL (or flattened `task.eligible` to a bare
// `task_eligible`) instead of under `metadata.hugen.task`. The
// frontmatter then PARSES (the misplaced keys are silently dropped by
// the non-strict decoder), so the manifest looks valid but the skill
// is not task-eligible — no `task:<name>` tool, `schedule:create`
// rejects it. This validator turns that silent dud into an actionable
// error before the bundle is registered.
var ErrTaskBlockMisplaced = errors.New("skill: task block misplaced")

// ValidateTaskAuthoring inspects a parsed manifest for task-block
// placement mistakes the non-strict YAML decoder hides. It re-parses
// the preserved frontmatter ([Manifest.Raw]) into a generic map and
// flags the two shapes the dogfood produced:
//
//   - a TOP-LEVEL `task:` key (the canonical task block lives only at
//     `metadata.hugen.task`; a top-level one is dropped on parse);
//   - a flat `metadata.hugen.task_eligible` (the real field is
//     `metadata.hugen.task.eligible`).
//
// Either signal, while the parsed [Manifest.Hugen.Task.Eligible]
// stays false, means the author intended a task but mis-nested it.
// The returned error wraps [ErrTaskBlockMisplaced] and names the
// correct location so the model can self-correct and re-save.
//
// Returns nil when no misplacement signal is present — a manifest
// that legitimately is not task-eligible (a plain skill) passes
// untouched, as does a correctly-nested task block.
func ValidateTaskAuthoring(m Manifest) error {
	if len(m.Raw) == 0 {
		return nil
	}
	var generic map[string]any
	if _, err := yaml.Unmarshal(m.Raw, &generic, yaml.DecodeOpts{}); err != nil {
		// Raw already parsed once in Parse(); a failure here is not
		// ours to report — skip the placement check rather than mask
		// the real parse path.
		return nil
	}

	var problems []string
	if _, ok := generic["task"]; ok {
		problems = append(problems, "a top-level `task:` key (it is ignored — the task block lives ONLY under `metadata.hugen.task`)")
	}
	if hugen := nestedMap(generic, "metadata", "hugen"); hugen != nil {
		if _, ok := hugen["task_eligible"]; ok {
			problems = append(problems, "a flat `metadata.hugen.task_eligible` (the field is `metadata.hugen.task.eligible`)")
		}
	}
	if len(problems) == 0 {
		return nil
	}
	// A correctly-nested, eligible task block alongside a stray signal
	// is not fatal — the real block already works. Only flag when the
	// parsed block did NOT come through (the author's task intent was
	// lost), which is exactly the dead-task case.
	if m.Hugen.Task.Eligible {
		return nil
	}
	return fmt.Errorf("%w: found %s. Move the whole task block under `metadata.hugen.task` with `eligible: true`, `kind`, `goal_summary`, `inputs_schema`, and `allowed_tools_default` nested beneath it",
		ErrTaskBlockMisplaced, strings.Join(problems, " and "))
}

// nestedMap walks a chain of string keys through nested
// map[string]any values, returning the leaf map or nil if any hop is
// absent or not a map. Tolerant of the two shapes the YAML decoder
// emits (map[string]any from this package).
func nestedMap(root map[string]any, keys ...string) map[string]any {
	cur := root
	for _, k := range keys {
		next, ok := cur[k].(map[string]any)
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}
