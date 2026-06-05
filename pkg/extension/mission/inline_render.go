package mission

import (
	"fmt"
	"strings"
	"text/template"

	"github.com/hugr-lab/hugen/pkg/extension"
)

// renderRoleProse renders a role's `prompt` (which may carry Go
// `{{ .Inputs.<key> }}` template expressions) against the mission's
// merged spawn ⊕ resolved inputs BEFORE it lands in a task template's
// `[Your role]` slot — the same data shape inline plans use (Phase
// 6.1d). Static prompts (no `{{`) skip the parse. Best-effort: a
// broken template falls back to the raw string so a cosmetic authoring
// slip in a role prompt never wedges a wave (the literal `{{...}}`
// renders, which the author catches in dogfood). Phase B34.
func renderRoleProse(mission extension.SessionState, prose string) string {
	if prose == "" || !hasTemplate(prose) {
		return prose
	}
	var spawn, resolved map[string]any
	if m := FromState(mission); m != nil {
		spawn = m.SpawnInputs()
		_, resolved, _ = m.ResearchOutput()
	}
	out, err := renderInlineString(prose, inlineRenderData{Inputs: mergeInputs(spawn, resolved)})
	if err != nil {
		return prose
	}
	return out
}

// inlineRenderData is the dot value templates see when the runtime
// renders an inline plan's per-subagent fields. Carries the merged
// .Inputs map (spawn-time inputs overlaid with the research stage's
// ResolvedUserInputs, with research winning on key collision).
//
// Templates write `{{ .Inputs.<key> }}` to read from the merged map.
// The split is intentionally hidden — a subagent author doesn't need
// to know whether a value was passed via spawn_mission or gathered by
// the input-collector role, only that it's present in Inputs. Phase
// 6.1d.
type inlineRenderData struct {
	Inputs map[string]any
}

// renderInlinePlan returns a deep-copy of plan with every string-bearing
// subagent field (Skill, Task, Inputs string leaves) rendered as a Go
// template against the merged inputs map. Subagents flagged
// InputsFromResolved have their Inputs field replaced with the
// resolved map verbatim, overriding any literal Inputs value.
//
// Render failures abort the whole pass and bubble up — a broken
// template in a manifest is an authoring bug that should fail loud
// at mission start, not silently spawn a misconfigured worker.
//
// The merge order is spawnInputs first, ResolvedUserInputs on top
// (research-confirmed values override defaults). Both maps may be
// nil — a nil merged map is treated as empty, and templates using
// `{{ .Inputs.X }}` against a nil map produce zero values via
// `missingkey=zero`.
//
// Phase 6.1d.
func renderInlinePlan(plan *InlinePlan, spawnInputs, resolvedInputs map[string]any) (*InlinePlan, error) {
	if plan == nil {
		return nil, nil
	}
	merged := mergeInputs(spawnInputs, resolvedInputs)
	data := inlineRenderData{Inputs: merged}

	out := &InlinePlan{Waves: make([]Wave, 0, len(plan.Waves))}
	for i, wave := range plan.Waves {
		renderedSubs := make([]SubagentSpec, 0, len(wave.Subagents))
		for j, sub := range wave.Subagents {
			rs, err := renderSubagent(sub, data, resolvedInputs)
			if err != nil {
				return nil, fmt.Errorf("wave[%d] %q subagent[%d] %q: %w",
					i, wave.Label, j, sub.Name, err)
			}
			renderedSubs = append(renderedSubs, rs)
		}
		out.Waves = append(out.Waves, Wave{
			Label:              wave.Label,
			Subagents:          renderedSubs,
			SkipCheck:          wave.SkipCheck,
			AcceptanceCriteria: append([]string(nil), wave.AcceptanceCriteria...),
		})
	}
	return out, nil
}

// renderSubagent renders the template-bearing fields of one
// SubagentSpec. Returns a copy — the input spec is never mutated.
// InputsFromResolved subagents short-circuit Inputs templating and
// take the resolved map verbatim.
func renderSubagent(sub SubagentSpec, data inlineRenderData, resolved map[string]any) (SubagentSpec, error) {
	out := sub
	out.DependsOn = append([]string(nil), sub.DependsOn...)

	if sub.InputsFromResolved && sub.Inputs != nil {
		return SubagentSpec{}, fmt.Errorf(
			"inputs_from_resolved=true is mutually exclusive with literal inputs",
		)
	}

	if hasTemplate(sub.Skill) {
		rendered, err := renderInlineString(sub.Skill, data)
		if err != nil {
			return SubagentSpec{}, fmt.Errorf("skill: %w", err)
		}
		out.Skill = rendered
	}
	if hasTemplate(sub.Task) {
		rendered, err := renderInlineString(sub.Task, data)
		if err != nil {
			return SubagentSpec{}, fmt.Errorf("task: %w", err)
		}
		out.Task = rendered
	}

	if sub.InputsFromResolved {
		out.Inputs = copyResolvedInputs(resolved)
		return out, nil
	}

	renderedInputs, err := renderInlineValue(sub.Inputs, data)
	if err != nil {
		return SubagentSpec{}, fmt.Errorf("inputs: %w", err)
	}
	out.Inputs = renderedInputs
	return out, nil
}

// renderInlineValue walks an arbitrary structured value and renders
// any string leaves containing a template marker. Map and slice
// containers are recreated so the caller's input is never mutated.
func renderInlineValue(v any, data inlineRenderData) (any, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case string:
		if !hasTemplate(t) {
			return t, nil
		}
		return renderInlineString(t, data)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			rendered, err := renderInlineValue(vv, data)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			out[k] = rendered
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			rendered, err := renderInlineValue(vv, data)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			out[i] = rendered
		}
		return out, nil
	default:
		return v, nil
	}
}

// renderInlineString parses and executes a single template against
// data. Uses `missingkey=zero` so absent input keys don't abort with
// a hard error; for a `map[string]any` value type the zero is nil
// which text/template prints as `<no value>`, so we post-process
// that marker to an empty string for the user-facing UX (a worker
// receiving a Task body with `<no value>` inline is jarring).
func renderInlineString(body string, data inlineRenderData) (string, error) {
	t, err := template.New("inline").Option("missingkey=zero").Parse(body)
	if err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	var buf strings.Builder
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute: %w", err)
	}
	return strings.ReplaceAll(buf.String(), "<no value>", ""), nil
}

// hasTemplate fast-paths fields that contain no template markers so
// the runtime skips parse/execute overhead on the common static case.
func hasTemplate(s string) bool {
	return strings.Contains(s, "{{")
}

// mergeInputs returns a fresh map combining spawn and resolved
// inputs. Resolved wins on key collision (research-confirmed values
// override the defaults the caller supplied at spawn_mission time).
// Returns nil when both inputs are empty so templates can rely on
// `missingkey=zero` to render absent keys as empty strings.
func mergeInputs(spawn, resolved map[string]any) map[string]any {
	if len(spawn) == 0 && len(resolved) == 0 {
		return nil
	}
	out := make(map[string]any, len(spawn)+len(resolved))
	for k, v := range spawn {
		out[k] = v
	}
	for k, v := range resolved {
		out[k] = v
	}
	return out
}

// copyResolvedInputs returns a defensive copy of the resolved-inputs
// map so the rendered SubagentSpec carries a value isolated from
// MissionState's internal buffer.
func copyResolvedInputs(resolved map[string]any) map[string]any {
	if len(resolved) == 0 {
		return nil
	}
	out := make(map[string]any, len(resolved))
	for k, v := range resolved {
		out[k] = v
	}
	return out
}
