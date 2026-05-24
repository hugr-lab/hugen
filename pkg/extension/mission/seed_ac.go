package mission

import (
	"fmt"
	"strings"
	"text/template"
)

// seedManifestAC renders each statement in manifest.AcceptanceCriteria
// as a Go template against `.Inputs` (the structured spawn-time input
// map), then seeds the resulting strings into state.AC with
// origin=manifest, iter=0. No-op when the manifest carries no AC
// seed.
//
// Render failures abort the whole seed and bubble up — a broken AC
// template is a mission-author bug that should fail loud at spawn,
// not silently leave the mission with a partial contract.
//
// The render data shape mirrors the planner_task.tmpl convention:
// templates reference `.Inputs.X` (e.g. `{{.Inputs.SourceID}}`) to
// pick up values from the spawn-time payload. When inputs is nil the
// template still renders — `.Inputs` resolves to nil and `{{.Inputs.X}}`
// produces an empty string (Go template default).
//
// Phase 5.x — B11 §3.2.2.
func seedManifestAC(m *MissionState, manifest MissionManifest, inputs any) error {
	if len(manifest.AcceptanceCriteria) == 0 {
		return nil
	}
	data := struct{ Inputs any }{Inputs: inputs}
	items := make([]ACAddSpec, 0, len(manifest.AcceptanceCriteria))
	for i, raw := range manifest.AcceptanceCriteria {
		rendered, err := renderACTemplate(raw, data)
		if err != nil {
			return fmt.Errorf("acceptance_criteria[%d]: %w", i, err)
		}
		if strings.TrimSpace(rendered) == "" {
			continue
		}
		items = append(items, ACAddSpec{Statement: rendered, Origin: OriginManifest})
	}
	if len(items) == 0 {
		return nil
	}
	m.SeedAC(items, OriginManifest)
	return nil
}

// renderACTemplate parses and executes a single AC statement template
// against data. Cheap one-shot — no caching since manifest seeds run
// once per mission spawn (cold path).
func renderACTemplate(raw string, data any) (string, error) {
	t, err := template.New("ac").Option("missingkey=zero").Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	var buf strings.Builder
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute: %w", err)
	}
	return buf.String(), nil
}
