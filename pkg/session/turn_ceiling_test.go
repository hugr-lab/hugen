package session

import (
	"context"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// TestSoftWarningText_RoleConditioned pins the spec wording: the root
// variant nudges toward sub-agent decomposition; the sub-agent variant
// reminds the model that sub-sub-agents aren't available at depth.
func TestSoftWarningText_RoleConditioned(t *testing.T) {
	root := softWarningText("root", "", 7)
	if !strings.Contains(root, "spawn_subagent") {
		t.Errorf("root variant missing spawn_subagent hint: %q", root)
	}
	if !strings.Contains(root, "soft signal") {
		t.Errorf("root variant missing 'soft signal' phrasing: %q", root)
	}

	sub := softWarningText("explorer", "list sources", 7)
	if !strings.Contains(sub, `"list sources"`) {
		t.Errorf("sub-agent variant missing task quote: %q", sub)
	}
	if !strings.Contains(sub, "Sub-sub-agents are not available") {
		t.Errorf("sub-agent variant missing depth notice: %q", sub)
	}
	if strings.Contains(sub, "spawn_subagent") {
		t.Errorf("sub-agent variant should not advertise spawn_subagent: %q", sub)
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
