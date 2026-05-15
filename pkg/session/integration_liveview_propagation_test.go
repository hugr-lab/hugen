package session

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension/liveview"
	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// TestLiveview_ChildFramesPropagateThroughPumpToParentOutbox is the
// end-to-end check for the TUI sidebar's "Subagents" section:
//
//  1. Parent + child both have a liveview extension registered.
//  2. Child emits a frame; child's liveview emits a status frame on
//     child.Outbox.
//  3. The parent's consumeChildOutbox pump receives that status frame
//     and calls notifyChildFrameObservers, which dispatches to
//     parent's liveview.OnChildFrame.
//  4. Parent's observer folds the child's status into its own
//     children map and force-emits on parent.Outbox.
//  5. The emitted parent frame's payload contains a non-empty
//     `children` map keyed by the child session id.
//
// If this regresses, the TUI shows an empty sidebar Subagents section
// during multi-tier sessions.
func TestLiveview_ChildFramesPropagateThroughPumpToParentOutbox(t *testing.T) {
	store := fixture.NewTestStore()
	ext := liveview.New(nil)
	parent, cleanup := newTestParent(t,
		withTestStore(store),
		withTestExtensions(ext),
		withTestRunLoop(),
	)
	defer cleanup()

	ctx := context.Background()
	child, err := parent.Spawn(ctx, SpawnSpec{Task: "test-mission"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	// Drain SubagentStarted from parent's outbox so the assertion
	// below picks up the liveview/status frame specifically.
	drainOutboxOnce(parent.Outbox())

	// Drive a frame through the child so its liveview folds something
	// "lifecycle-changing" and force-emits a status frame onto
	// child.Outbox.
	if err := child.emit(ctx,
		protocol.NewToolCall(child.ID(), protocol.ParticipantInfo{ID: "a1", Kind: protocol.ParticipantAgent},
			"tc-1", "test.tool", json.RawMessage(`{}`)),
	); err != nil {
		t.Fatalf("child emit: %v", err)
	}

	// The parent's pump consumes the child's status frame and the
	// parent's liveview reacts with its own status frame. Poll
	// parent.Outbox for a liveview/status frame carrying a non-empty
	// children map.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case f, ok := <-parent.Outbox():
			if !ok {
				t.Fatal("parent outbox closed without liveview frame")
			}
			ef, isExt := f.(*protocol.ExtensionFrame)
			if !isExt || ef.Payload.Extension != "liveview" || ef.Payload.Op != "status" {
				continue
			}
			var body map[string]any
			if err := json.Unmarshal(ef.Payload.Data, &body); err != nil {
				t.Fatalf("parent liveview payload unmarshal: %v", err)
			}
			kids, ok := body["children"].(map[string]any)
			if !ok || len(kids) == 0 {
				continue
			}
			if _, ok := kids[child.ID()]; !ok {
				t.Fatalf("parent emit carries children map without child id %q: %+v",
					child.ID(), kids)
			}
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("parent never emitted a liveview/status frame with non-empty children map within 3s")
}
