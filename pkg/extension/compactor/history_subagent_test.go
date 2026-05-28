package compactor

import (
	"testing"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// TestSubagentStartedNotRenderedIntoHistory pins the fix for the
// Gemini 400 "function response turn comes immediately after a
// function call turn". A tool-driven spawn (recipe `task:*`, sync
// `spawn_mission`) emits subagent_started WHILE the dispatcher
// blocks, so the row lands between the tool's function_call and its
// function_response. Rendering it as a RoleUser turn split the
// call/response pair and strict providers rejected the request. The
// note carries no model-relevant information (the spawn tool's
// result + the later subagent_result cover it), so both projection
// paths must skip it. subagent_result must still render.
func TestSubagentStartedNotRenderedIntoHistory(t *testing.T) {
	renderer := productionRendererForCompactor(t)

	t.Run("frame path skips SubagentStarted", func(t *testing.T) {
		frame := protocol.NewSubagentStarted("ses-parent", protocol.ParticipantInfo{},
			protocol.SubagentStartedPayload{
				ChildSessionID: "ses-child",
				Role:           "worker",
				Task:           "Run the recipe once.",
				Depth:          1,
			})
		if _, ok := projectFrameToEntry(renderer, frame); ok {
			t.Fatalf("projectFrameToEntry rendered subagent_started into model history; want skipped")
		}
	})

	t.Run("row path skips SubagentStarted", func(t *testing.T) {
		row := &store.EventRow{
			Seq:       14,
			EventType: string(protocol.KindSubagentStarted),
			Metadata: map[string]any{
				"child_session_id": "ses-child",
				"role":             "worker",
				"depth":            1,
			},
		}
		if _, ok := projectRowToEntry(renderer, row); ok {
			t.Fatalf("projectRowToEntry rendered subagent_started into model history; want skipped")
		}
	})

	t.Run("subagent_result still renders", func(t *testing.T) {
		row := &store.EventRow{
			Seq:       16,
			EventType: string(protocol.KindSubagentResult),
			Content:   "counted: 4231 rows",
			Metadata: map[string]any{
				"session_id": "ses-child",
				"reason":     "completed",
			},
		}
		if _, ok := projectRowToEntry(renderer, row); !ok {
			t.Fatalf("projectRowToEntry skipped subagent_result; want rendered")
		}
	})
}
