package session

import (
	"context"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// TestSoftWarningText_RoleConditioned pins the spec wording across
// the three audiences: root (always nudge to spawn_subagent),
// sub-agent with depth budget (suggest fan-out), sub-agent at the
// max-depth floor (omit fan-out, point at return / give up / change
// tack).
func TestSoftWarningText_RoleConditioned(t *testing.T) {
	root := softWarningText("root", "", 7, true)
	if !strings.Contains(root, "spawn_subagent") {
		t.Errorf("root variant missing spawn_subagent hint: %q", root)
	}
	if !strings.Contains(root, "soft signal") {
		t.Errorf("root variant missing 'soft signal' phrasing: %q", root)
	}

	subDeeper := softWarningText("explorer", "list sources", 7, true)
	if !strings.Contains(subDeeper, `"list sources"`) {
		t.Errorf("sub-agent (depth ok) variant missing task quote: %q", subDeeper)
	}
	if !strings.Contains(subDeeper, "spawn_subagent") {
		t.Errorf("sub-agent (depth ok) variant should mention spawn_subagent: %q", subDeeper)
	}
	if strings.Contains(subDeeper, "Sub-sub-agents are not available") {
		t.Errorf("sub-agent (depth ok) variant must not say sub-sub-agents are unavailable: %q", subDeeper)
	}

	subFloor := softWarningText("explorer", "list sources", 7, false)
	if !strings.Contains(subFloor, `"list sources"`) {
		t.Errorf("sub-agent (at depth) variant missing task quote: %q", subFloor)
	}
	if !strings.Contains(subFloor, "Sub-sub-agents are not available") {
		t.Errorf("sub-agent (at depth) variant missing depth notice: %q", subFloor)
	}
	if strings.Contains(subFloor, "spawn_subagent") {
		t.Errorf("sub-agent (at depth) variant should not advertise spawn_subagent: %q", subFloor)
	}
}

// TestReloadSoftWarningFlag_RestartIdempotency confirms a session that
// already emitted the soft-warning event in its log boots with the
// once-per-session flag set, so a post-restart turn boundary skips
// re-emission. The complementary "no event → flag stays false" path
// is also covered.
func TestReloadSoftWarningFlag_RestartIdempotency(t *testing.T) {
	s := &Session{}
	s.reloadSoftWarningFlag(nil)
	if s.softWarningDone.Load() {
		t.Fatalf("empty event log should not flip the once-per-session flag")
	}

	rows := []EventRow{
		{EventType: string(protocol.KindSystemMessage), Metadata: map[string]any{"kind": protocol.SystemMessageStuckNudge}},
		{EventType: string(protocol.KindSystemMessage), Metadata: map[string]any{"kind": protocol.SystemMessageSoftWarning}},
	}
	s.reloadSoftWarningFlag(rows)
	if !s.softWarningDone.Load() {
		t.Errorf("soft-warning event in log → flag should be set after reload")
	}
}

// TestResolveHardCeiling_DefaultsToTwoTimesSoft pins the spec default
// (max_turns_hard defaults to 2 × max_turns) when no skill manifest
// overrides it. Sessions without a SkillManager fall back to the
// runtime default cap × 2 — verified separately to keep the matrix
// honest.
func TestResolveHardCeiling_DefaultsToTwoTimesSoft(t *testing.T) {
	s := &Session{}

	if got, want := s.resolveHardCeiling(context.Background(), 5), 10; got != want {
		t.Errorf("resolveHardCeiling(5) = %d, want %d", got, want)
	}
	if got, want := s.resolveHardCeiling(context.Background(), 0), defaultMaxToolIterations*2; got != want {
		t.Errorf("resolveHardCeiling(0) = %d, want %d", got, want)
	}
}

// TestStuckDetectionEnabled_DefaultsTrue verifies a session with no
// SkillManager (the no-skill deployment path) keeps stuck detection
// active by default — operators opt out via skill manifest.
func TestStuckDetectionEnabled_DefaultsTrue(t *testing.T) {
	s := &Session{}
	if !s.stuckDetectionEnabled(context.Background()) {
		t.Errorf("stuckDetectionEnabled = false on no-skill session, want true")
	}
}
