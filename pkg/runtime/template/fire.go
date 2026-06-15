// Package template hosts the text/template machinery the scheduler
// uses to interpolate per-fire context into cron-task templates —
// goal, inputs map, system-prompt blocks. It lives under pkg/runtime
// (not pkg/extension/scheduler) so other phase-6+ consumers (memory
// pipeline, future workspace digests) can reuse the same funcmap +
// FireRenderContext shape without taking a dep on the scheduler
// extension.
//
// Two rendering surfaces today:
//
//   - [RenderTemplate]    — generic entrypoint a caller uses when it
//     already holds the template body as a string (e.g. the cron
//     fire fn rendering `tasks.spec.goal` into the first UserMessage).
//   - [RenderInto]        — a thin wrapper that accepts a pre-parsed
//     `*template.Template` so the scheduler can amortise parsing on
//     bootstrap-time registration paths.
//
// The funcmap is intentionally small: `now`, `formatDate`,
// `addDuration`, `default`. Anything richer goes through Go-side
// rendering before the template fires. Phase 6 spec §1.2.4.
package template

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// FireRenderContext is the dot value templates see when the scheduler
// renders a per-fire string (goal, system-prompt block, inputs
// preface). It is a flat projection of [protocol.FireContext] so
// templates can write `{{ .Goal }}`, `{{ .Inputs.region }}`,
// `{{ .PrevFire.Summary }}` without nested-pointer juggling.
//
// Producers MUST go through [NewFireRenderContext] so missing /
// nil substructures (no PrevFire on the first fire, empty Inputs)
// degrade to harmless zero values rather than tripping template
// nil dereferences. Phase 6 spec §1.2.4.
type FireRenderContext struct {
	// TaskID identifies the source task row. Surfaces in audit
	// blocks the model echoes back into notification bodies.
	TaskID string

	// FireSeq is the 1-indexed per-task fire counter. Templates
	// use it to emit "fire #3 of …" style headers.
	FireSeq int

	// PlannedAt is the schedule-target instant for this fire,
	// already converted to UTC. Rendered via `formatDate`.
	PlannedAt time.Time

	// FireTime is the wall-clock instant at which rendering
	// happens. Distinct from PlannedAt: a fire that ticks late
	// or that retries after a transient failure carries a
	// FireTime > PlannedAt. Templates that surface "rendered at
	// …" copy use this; "scheduled at …" copy uses PlannedAt.
	FireTime time.Time

	// Goal is the imperative one-line brief from
	// `tasks.spec.goal`. Empty for `wake` kind tasks (which use
	// WakeMessage on the fire fn side, not the renderer).
	Goal string

	// Inputs is the structured per-task parameter blob —
	// keys interpolate via `{{ .Inputs.<key> }}`. Always
	// non-nil after NewFireRenderContext (empty map when the
	// task carries no inputs) so templates can dot into it
	// without nil-checks.
	Inputs map[string]any

	// PrevFire is the prior successful fire's outcome, or nil
	// when this is the first successful fire. Templates that
	// reference `.PrevFire.Summary` MUST gate on
	// `{{ if .PrevFire }}` — that's a per-template policy
	// since "last result" prose varies.
	PrevFire *protocol.PrevFireOutcome

	// AllowedTools is the per-task tool allow-list pre-stamped
	// at task-create time. Rendered into the system prompt's
	// audit block; CronApprovalPolicy is the load-bearing
	// enforcer, the template is informational.
	AllowedTools []string
}

// NewFireRenderContext builds the dot value from a runtime
// [protocol.FireContext] envelope. Performs the nil-to-empty-map
// hardening and stamps FireTime to time.Now() so callers don't have
// to remember.
func NewFireRenderContext(fc *protocol.FireContext) FireRenderContext {
	if fc == nil {
		return FireRenderContext{
			Inputs:   map[string]any{},
			FireTime: time.Now().UTC(),
		}
	}
	inputs := fc.Inputs
	if inputs == nil {
		inputs = map[string]any{}
	}
	return FireRenderContext{
		TaskID:       fc.TaskID,
		FireSeq:      fc.FireSeq,
		PlannedAt:    fc.PlannedAt.UTC(),
		FireTime:     time.Now().UTC(),
		Goal:         fc.Goal,
		Inputs:       inputs,
		PrevFire:     fc.PrevFire,
		AllowedTools: fc.AllowedTools,
	}
}

// FuncMap returns the standard funcmap every fire template renders
// against. Adding entries here changes the surface every cron
// template can use — bump phase docs if you do.
func FuncMap() template.FuncMap {
	return template.FuncMap{
		"now": func() time.Time { return time.Now().UTC() },
		"formatDate": func(layout string, t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return t.UTC().Format(layout)
		},
		"addDuration": func(t time.Time, d string) (time.Time, error) {
			dur, err := time.ParseDuration(d)
			if err != nil {
				return time.Time{}, fmt.Errorf("addDuration: %w", err)
			}
			return t.Add(dur), nil
		},
		"default": func(fallback, v any) any {
			if v == nil {
				return fallback
			}
			if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
				return fallback
			}
			return v
		},
	}
}

// RenderTemplate parses body as a text/template against [FuncMap]
// and executes it with ctx as dot. Returns the rendered string +
// any parse / execution error. The template name is "fire" for
// readable error messages.
//
// Use [RenderInto] when the caller can hold onto a parsed
// `*template.Template` (e.g. amortised across many fires of the
// same task).
func RenderTemplate(body string, ctx FireRenderContext) (string, error) {
	tmpl, err := template.New("fire").Funcs(FuncMap()).Parse(body)
	if err != nil {
		return "", fmt.Errorf("template parse: %w", err)
	}
	return RenderInto(tmpl, ctx)
}

// RenderInto executes a pre-parsed template against ctx. Callers
// that don't need amortisation should use [RenderTemplate]; this
// surface exists so the scheduler can keep a per-task parsed
// template alongside the registration to avoid re-parsing on every
// fire.
func RenderInto(tmpl *template.Template, ctx FireRenderContext) (string, error) {
	if tmpl == nil {
		return "", fmt.Errorf("template: nil template")
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, ctx); err != nil {
		return "", fmt.Errorf("template exec: %w", err)
	}
	return buf.String(), nil
}

// RenderInputs renders every string value in a per-task inputs blob
// against ctx, so per-fire template vars ({{.FireSeq}}, {{.FireTime}})
// embedded in an input value resolve at fire time instead of reaching
// the task as a literal. The motivating case (Phase 6 spec §D7): a
// task input `output_path: "report_{{.FireSeq}}.html"` must produce a
// distinct path per fire — otherwise every fire overwrites one file.
//
// Walks nested maps and slices; non-string leaves and strings with no
// `{{` action pass through untouched (so a naive `<time>` placeholder
// stays literal). Returns a NEW map — the stored task spec is never
// mutated. A parse/exec failure surfaces with the offending key path
// so the scheduler can pause the task with an actionable reason.
func RenderInputs(inputs map[string]any, ctx FireRenderContext) (map[string]any, error) {
	if inputs == nil {
		return nil, nil
	}
	// Always allocate a fresh map (even for an empty input) so the
	// returned value never aliases the caller's stored spec — honouring
	// the "Returns a NEW map" contract regardless of size.
	out := make(map[string]any, len(inputs))
	for k, v := range inputs {
		rv, err := renderInputValue(v, ctx)
		if err != nil {
			return nil, fmt.Errorf("inputs.%s: %w", k, err)
		}
		out[k] = rv
	}
	return out, nil
}

// renderInputValue renders a single inputs value, recursing through
// maps and slices and rendering string leaves that carry a template
// action.
func renderInputValue(v any, ctx FireRenderContext) (any, error) {
	switch t := v.(type) {
	case string:
		if !strings.Contains(t, "{{") {
			return t, nil
		}
		return RenderTemplate(t, ctx)
	case map[string]any:
		return RenderInputs(t, ctx)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			rv, err := renderInputValue(e, ctx)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			out[i] = rv
		}
		return out, nil
	default:
		return v, nil
	}
}
