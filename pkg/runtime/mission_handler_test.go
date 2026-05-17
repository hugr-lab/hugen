package runtime

import (
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/session"
)

// TestRenderMissionsList_Empty covers the no-children case.
func TestRenderMissionsList_Empty(t *testing.T) {
	got := renderMissionsList(nil)
	if got != "No missions running." {
		t.Errorf("renderMissionsList(nil) = %q", got)
	}
}

// TestRenderMissionsList_Populated covers the formatted-list case.
func TestRenderMissionsList_Populated(t *testing.T) {
	snaps := []session.ChildSnapshot{
		{SessionID: "ses-a", Name: "alpha", Depth: 1},
		{SessionID: "ses-b", Name: "beta", Depth: 1},
	}
	got := renderMissionsList(snaps)
	if !strings.Contains(got, "2 mission(s) running:") {
		t.Errorf("count line missing: %q", got)
	}
	if !strings.Contains(got, "alpha") || !strings.Contains(got, "beta") {
		t.Errorf("mission names missing: %q", got)
	}
	if !strings.Contains(got, "ses-a") || !strings.Contains(got, "ses-b") {
		t.Errorf("session_ids missing: %q", got)
	}
}

// TestFindMission_ByName resolves a target string against snapshots
// by Name precedence.
func TestFindMission_ByName(t *testing.T) {
	snaps := []session.ChildSnapshot{
		{SessionID: "ses-a", Name: "alpha", Depth: 1},
		{SessionID: "ses-b", Name: "beta", Depth: 1},
	}
	got, ok := findMission(snaps, "beta")
	if !ok || got.Name != "beta" || got.SessionID != "ses-b" {
		t.Errorf("findMission(beta) = (%+v, %v)", got, ok)
	}
}

// TestFindMission_BySessionID falls through to session_id when no
// name matches.
func TestFindMission_BySessionID(t *testing.T) {
	snaps := []session.ChildSnapshot{
		{SessionID: "ses-a", Name: "alpha", Depth: 1},
	}
	got, ok := findMission(snaps, "ses-a")
	if !ok || got.SessionID != "ses-a" {
		t.Errorf("findMission(ses-a) = (%+v, %v)", got, ok)
	}
}

// TestFindMission_Miss returns ok=false when neither lookup hits.
func TestFindMission_Miss(t *testing.T) {
	snaps := []session.ChildSnapshot{
		{SessionID: "ses-a", Name: "alpha", Depth: 1},
	}
	_, ok := findMission(snaps, "missing")
	if ok {
		t.Errorf("findMission(missing) ok=true")
	}
}

// TestMissionsList_PayloadShape captures the SystemMarker payload
// the handler emits, to anchor scenario assertions.
func TestMissionsList_PayloadShape(t *testing.T) {
	snaps := []session.ChildSnapshot{
		{SessionID: "ses-a", Name: "alpha", Depth: 1},
	}
	rows := missionsList(snaps)
	if len(rows) != 1 {
		t.Fatalf("missionsList len = %d, want 1", len(rows))
	}
	got := rows[0]
	if got["session_id"] != "ses-a" {
		t.Errorf("session_id = %v, want ses-a", got["session_id"])
	}
	if got["name"] != "alpha" {
		t.Errorf("name = %v, want alpha", got["name"])
	}
	if got["depth"] != 1 {
		t.Errorf("depth = %v, want 1", got["depth"])
	}
}
