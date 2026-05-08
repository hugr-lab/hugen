package session

import (
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// projectHistory unit test — keeps the most-recent K user/agent
// messages.
func TestProjectHistory_Window(t *testing.T) {
	rows := make([]EventRow, 0, 200)
	for i := 0; i < 100; i++ {
		rows = append(rows, EventRow{
			EventType: string(protocol.KindUserMessage),
			Content:   "user",
		})
		rows = append(rows, EventRow{
			EventType: string(protocol.KindAgentMessage),
			Content:   "agent",
			Metadata:  map[string]any{"final": true},
		})
	}
	got := projectHistory(rows, 50)
	if len(got) != 50 {
		t.Errorf("len = %d, want 50", len(got))
	}
}

// TestProjectHistory_IncludesSubagentFrames verifies phase-4 US6:
// subagent_started and subagent_result events replay into history
// with the same "[system: spawned_note] ..." / "[system:
// subagent_result] ... reason=... turns=..." rendering the live
// visibility filter (visibility.go projectFrameToHistory) uses.
// Without this the synthetic settle subagent_result rows written by
// settleDanglingSubagents would be invisible to the parent's model
// after a process restart.
func TestProjectHistory_IncludesSubagentFrames(t *testing.T) {
	rows := []EventRow{
		{
			EventType: string(protocol.KindSubagentStarted),
			Content:   "explore the catalog",
			Metadata: map[string]any{
				"child_session_id": "sub-c1",
				"role":             "explorer",
				"depth":            float64(1),
				"task":             "explore the catalog",
			},
		},
		{
			EventType: string(protocol.KindSubagentResult),
			Content:   "Sub-agent sub-c1 did not deliver a result before the previous process exited.",
			Metadata: map[string]any{
				"session_id": "sub-c1",
				"reason":     protocol.TerminationRestartDied,
				"turns_used": float64(0),
			},
		},
	}
	got := projectHistory(rows, 50)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2; got=%v", len(got), got)
	}
	if !strings.HasPrefix(got[0].Content,
		"[system: "+protocol.SystemMessageSpawnedNote+"]") {
		t.Errorf("subagent_started replay = %q, want spawned_note prefix", got[0].Content)
	}
	if !strings.HasPrefix(got[1].Content, "[system: subagent_result]") {
		t.Errorf("subagent_result replay = %q, want subagent_result prefix", got[1].Content)
	}
	if !strings.Contains(got[1].Content, protocol.TerminationRestartDied) {
		t.Errorf("subagent_result replay = %q, missing reason", got[1].Content)
	}
}

// TestProjectHistory_IncludesSystemMessage verifies the phase-4 US6
// extension: system_message rows replay into history under the
// canonical "[system: <kind>] <content>" prefix so runtime-injected
// notices (soft_warning, stuck_nudge, whiteboard) survive a process
// restart's materialise. Read shape matches the live visibility
// filter (visibility.go) so the model sees identical text whether
// the frame arrived live or on replay.
func TestProjectHistory_IncludesSystemMessage(t *testing.T) {
	rows := []EventRow{
		{EventType: string(protocol.KindUserMessage), Content: "hi"},
		{
			EventType: string(protocol.KindSystemMessage),
			Content:   "sub-agent X died on restart; Y respawned.",
			Metadata:  map[string]any{"kind": protocol.SystemMessageSpawnedNote},
		},
		{
			EventType: string(protocol.KindAgentMessage),
			Content:   "ack",
			Metadata:  map[string]any{"final": true},
		},
	}
	got := projectHistory(rows, 50)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (user + system + agent); got=%v", len(got), got)
	}
	if got[1].Role != model.RoleUser {
		t.Errorf("system_message Role = %v, want RoleUser", got[1].Role)
	}
	wantPrefix := "[system: " + protocol.SystemMessageSpawnedNote + "] "
	if !strings.HasPrefix(got[1].Content, wantPrefix) {
		t.Errorf("system_message Content = %q, want prefix %q", got[1].Content, wantPrefix)
	}
}
