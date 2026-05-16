package session

import (
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// TestProjectFrameToHistory_AllowList walks the §11 allow-list and
// asserts each Frame variant either projects to a model.Message with
// the prescribed `[system: <kind>]` prefix or is denied. The default-
// deny default is exercised by the *Reasoning case at the bottom.
func TestProjectFrameToHistory_AllowList(t *testing.T) {
	author := protocol.ParticipantInfo{ID: "ag", Kind: protocol.ParticipantAgent}
	user := protocol.ParticipantInfo{ID: "u1", Kind: protocol.ParticipantUser}

	cases := []struct {
		name        string
		frame       protocol.Frame
		wantAllowed bool
		wantPrefix  string
	}{
		{
			name:        "user_message passes verbatim",
			frame:       protocol.NewUserMessage("s1", user, "hello"),
			wantAllowed: true,
			wantPrefix:  "hello",
		},
		{
			name: "subagent_started → spawned_note",
			frame: protocol.NewSubagentStarted("s1", author, protocol.SubagentStartedPayload{
				ChildSessionID: "child-1", Skill: "hugr-data", Role: "explorer",
				Task: "list", Depth: 1, StartedAt: time.Now(),
			}),
			wantAllowed: true,
			wantPrefix:  "[system: " + protocol.SystemMessageSpawnedNote + "]",
		},
		{
			name: "subagent_result with body",
			frame: protocol.NewSubagentResult("s1", "child-1", author, protocol.SubagentResultPayload{
				SessionID: "child-1", Result: "found foo", Reason: protocol.TerminationCompleted, TurnsUsed: 4,
			}),
			wantAllowed: true,
			wantPrefix:  "[system: subagent_result]",
		},
		{
			// Phase 5.2 τ — Parked=true renders with the explicit
			// "still alive, awaiting directive" framing so the
			// model doesn't read the row as terminal.
			name: "subagent_result with Parked=true renders parked template",
			frame: protocol.NewSubagentResult("s1", "child-1", author, protocol.SubagentResultPayload{
				SessionID: "child-1", Result: "found foo",
				Reason: protocol.TerminationCompleted, TurnsUsed: 4, Parked: true,
			}),
			wantAllowed: true,
			wantPrefix:  "state=parked",
		},
		{
			name: "subagent_result without body falls back to reason",
			frame: protocol.NewSubagentResult("s1", "child-1", author, protocol.SubagentResultPayload{
				SessionID: "child-1", Reason: protocol.TerminationHardCeiling, TurnsUsed: 60,
			}),
			wantAllowed: true,
			wantPrefix:  "(no result; reason: " + protocol.TerminationHardCeiling + ")",
		},
		{
			name: "system_message renders [system: <kind>]",
			frame: protocol.NewSystemMessage("s1", author,
				protocol.SystemMessageSoftWarning, "consider stopping"),
			wantAllowed: true,
			wantPrefix:  "[system: " + protocol.SystemMessageSoftWarning + "]",
		},
		{
			name:        "reasoning frame denied (default-deny)",
			frame:       protocol.NewReasoning("s1", author, "thinking…", 0, false),
			wantAllowed: false,
		},
		{
			name: "tool_call frame denied",
			frame: protocol.NewToolCall("s1", author, "tc1", "fake:do",
				map[string]any{"x": 1}),
			wantAllowed: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, ok := projectFrameToHistory(testPrompts(t), tc.frame)
			if ok != tc.wantAllowed {
				t.Fatalf("allowed = %v, want %v", ok, tc.wantAllowed)
			}
			if !ok {
				if visibilityAllows(tc.frame) {
					t.Errorf("visibilityAllows reports allow=true for denied frame")
				}
				return
			}
			if !visibilityAllows(tc.frame) {
				t.Errorf("visibilityAllows reports allow=false for allowed frame")
			}
			if !strings.Contains(msg.Content, tc.wantPrefix) {
				t.Errorf("Content = %q, want substring %q", msg.Content, tc.wantPrefix)
			}
		})
	}
}
