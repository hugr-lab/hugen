package mission

import (
	"strings"
	"testing"
)

func TestSeedManifestAC_NoSeed(t *testing.T) {
	m := NewMissionState()
	if err := seedManifestAC(m, MissionManifest{}, nil); err != nil {
		t.Fatalf("no-seed should be a no-op: %v", err)
	}
	if got := m.ACSnapshot(); len(got) != 0 {
		t.Errorf("unexpected AC after no-seed: %+v", got)
	}
}

func TestSeedManifestAC_TemplateRendersWithInputs(t *testing.T) {
	m := NewMissionState()
	manifest := MissionManifest{
		AcceptanceCriteria: []string{
			"Task row created for {{.Inputs.SourceID}}",
			"Schedule expression parses for kind={{.Inputs.Kind}}",
		},
	}
	inputs := map[string]any{
		"SourceID": "inventory-prod",
		"Kind":     "cron",
	}
	if err := seedManifestAC(m, manifest, inputs); err != nil {
		t.Fatalf("seedManifestAC: %v", err)
	}
	rows := m.ACSnapshot()
	if len(rows) != 2 {
		t.Fatalf("expected 2 seeded rows, got %d", len(rows))
	}
	if !strings.Contains(rows[0].Statement, "inventory-prod") {
		t.Errorf("row 0 should render SourceID: %q", rows[0].Statement)
	}
	if !strings.Contains(rows[1].Statement, "kind=cron") {
		t.Errorf("row 1 should render Kind: %q", rows[1].Statement)
	}
	if rows[0].Origin != OriginManifest {
		t.Errorf("origin=%q, want manifest", rows[0].Origin)
	}
	if rows[0].AddedAtIter != 0 {
		t.Errorf("AddedAtIter=%d, want 0 (iter-0 seed)", rows[0].AddedAtIter)
	}
}

func TestSeedManifestAC_MissingInputDoesNotPanic(t *testing.T) {
	m := NewMissionState()
	manifest := MissionManifest{
		AcceptanceCriteria: []string{"Schedule kind: {{.Inputs.MissingKey}}"},
	}
	// missingkey=zero renders the field as empty string instead of
	// panicking — manifests SHOULD render even if a caller skipped
	// the field.
	if err := seedManifestAC(m, manifest, map[string]any{}); err != nil {
		t.Fatalf("missing key should render as empty, got error: %v", err)
	}
	rows := m.ACSnapshot()
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	// Expect "Schedule kind: " (trailing whitespace from missingkey=zero
	// rendering as empty). Confirm the constant prefix made it through.
	if !strings.HasPrefix(rows[0].Statement, "Schedule kind: ") {
		t.Errorf("statement should keep constant prefix: %q", rows[0].Statement)
	}
}

func TestSeedManifestAC_BadTemplateAborts(t *testing.T) {
	m := NewMissionState()
	manifest := MissionManifest{
		AcceptanceCriteria: []string{
			"Valid: {{.Inputs.X}}",
			"Broken: {{.Inputs.Unclosed", // missing closing braces
		},
	}
	err := seedManifestAC(m, manifest, map[string]any{"X": "ok"})
	if err == nil {
		t.Fatal("broken template should produce an error")
	}
	if !strings.Contains(err.Error(), "acceptance_criteria[1]") {
		t.Errorf("error should reference the broken index: %v", err)
	}
	// Partial seed must be rolled back to avoid leaving the mission
	// in a half-contract state — SeedAC was never called.
	if rows := m.ACSnapshot(); len(rows) != 0 {
		t.Errorf("partial seed leaked into AC: %+v", rows)
	}
}

func TestSeedManifestAC_WhitespaceOnlyRendersDropped(t *testing.T) {
	m := NewMissionState()
	manifest := MissionManifest{
		AcceptanceCriteria: []string{
			"Real entry",
			"   ",
			"{{.Inputs.Empty}}",
		},
	}
	if err := seedManifestAC(m, manifest, map[string]any{"Empty": "   "}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rows := m.ACSnapshot()
	if len(rows) != 1 {
		t.Fatalf("want 1 surviving row (whitespace-only dropped), got %d: %+v", len(rows), rows)
	}
	if rows[0].Statement != "Real entry" {
		t.Errorf("statement=%q", rows[0].Statement)
	}
}
