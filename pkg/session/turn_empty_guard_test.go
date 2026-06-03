package session

import (
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// emptyTurn scripts one fully empty model iteration: a single Final
// chunk with no content and no tool call — the silent dead-turn the
// empty-iteration guard recovers from (observed on qwen3.6 in llama.cpp
// after a tool result with large tool-parameter schemas).
func emptyTurn() []model.Chunk { return []model.Chunk{{Final: true}} }

// TestEmptyIteration_RepromptsThenRecovers verifies that a content-less,
// tool-call-less model iteration is re-prompted (with the nudge injected
// as a system reminder) instead of retiring the turn empty-handed, and
// that the follow-up iteration's output is delivered.
func TestEmptyIteration_RepromptsThenRecovers(t *testing.T) {
	mdl := &scriptedToolModel{turns: [][]model.Chunk{
		emptyTurn(), // dead turn
		{{Content: ptr("recovered"), Final: true}}, // recovery after the re-prompt
	}}
	sess, cancel := newToolSession(t, mdl, permsAllow{})
	defer cancel()

	user := protocol.ParticipantInfo{ID: "u1", Kind: protocol.ParticipantUser}
	sess.Inbox() <- protocol.NewUserMessage("s1", user, "go")

	frames := collectFrames(t, sess, func(seen []protocol.Frame) bool {
		for _, f := range seen {
			if am, ok := f.(*protocol.AgentMessage); ok &&
				am.Payload.Final && am.Payload.Consolidated &&
				strings.Contains(am.Payload.Text, "recovered") {
				return true
			}
		}
		return false
	}, 3*time.Second)

	if mdl.calls.Load() != 2 {
		t.Errorf("model called %d times, want 2 (empty → re-prompt → recover)", mdl.calls.Load())
	}
	var nudged bool
	for _, f := range frames {
		if sm, ok := f.(*protocol.SystemMessage); ok && strings.Contains(sm.Payload.Content, "produced no output") {
			nudged = true
		}
	}
	if !nudged {
		t.Errorf("empty-iteration nudge not injected as a system_message: %v", kindNames(frames))
	}
}

// TestEmptyIteration_RetryCapRetires verifies a model that only ever
// returns empty iterations can't loop forever: the runtime re-prompts up
// to maxEmptyRetries times, then retires the turn (model called exactly
// maxEmptyRetries+1 times — the initial call plus the bounded retries).
func TestEmptyIteration_RetryCapRetires(t *testing.T) {
	turns := make([][]model.Chunk, maxEmptyRetries+3)
	for i := range turns {
		turns[i] = emptyTurn()
	}
	mdl := &scriptedToolModel{turns: turns}
	sess, cancel := newToolSession(t, mdl, permsAllow{})
	defer cancel()

	user := protocol.ParticipantInfo{ID: "u1", Kind: protocol.ParticipantUser}
	sess.Inbox() <- protocol.NewUserMessage("s1", user, "go")

	want := int32(maxEmptyRetries + 1)
	deadline := time.After(3 * time.Second)
	for mdl.calls.Load() < want {
		select {
		case <-deadline:
			t.Fatalf("model called only %d times, want %d before retire", mdl.calls.Load(), want)
		case <-time.After(10 * time.Millisecond):
		}
	}
	// Let the loop settle, then confirm it did NOT re-prompt past the cap.
	time.Sleep(100 * time.Millisecond)
	if got := mdl.calls.Load(); got != want {
		t.Errorf("model called %d times, want exactly %d (cap retires the turn)", got, want)
	}
}
