package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// TestRecover_OrphanSubagent_AppendsRestartDied verifies that a
// subagent row without a session_terminated event is closed out with
// reason "restart_died" and a synthetic subagent_result is appended
// to the parent's events.
func TestRecover_OrphanSubagent_AppendsRestartDied(t *testing.T) {
	store := newFakeStore()
	ctx := context.Background()

	now := time.Now().UTC()
	_ = store.OpenSession(ctx, SessionRow{
		ID: "root1", AgentID: "a1", SessionType: "root", Status: StatusActive,
		CreatedAt: now, UpdatedAt: now,
	})
	_ = store.OpenSession(ctx, SessionRow{
		ID: "sub1", AgentID: "a1", ParentSessionID: "root1", SessionType: "subagent", Status: StatusActive,
		Metadata:  map[string]any{"depth": 1},
		CreatedAt: now, UpdatedAt: now,
	})

	mgr := newTestManager(t, store)
	if _, err := Recover(ctx, mgr.deps); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	subEvents, _ := store.ListEvents(ctx, "sub1", ListEventsOpts{})
	if !containsKindWithReason(subEvents, protocol.KindSessionTerminated, protocol.TerminationRestartDied) {
		t.Errorf("subagent missing session_terminated{restart_died}: events=%v", kinds(subEvents))
	}

	parentEvents, _ := store.ListEvents(ctx, "root1", ListEventsOpts{})
	var foundResult bool
	for _, ev := range parentEvents {
		if ev.EventType == string(protocol.KindSubagentResult) {
			foundResult = true
			if ev.Metadata["session_id"] != "sub1" {
				t.Errorf("subagent_result.session_id = %v, want sub1", ev.Metadata["session_id"])
			}
			if ev.Metadata["reason"] != protocol.TerminationRestartDied {
				t.Errorf("subagent_result.reason = %v, want %s", ev.Metadata["reason"], protocol.TerminationRestartDied)
			}
		}
	}
	if !foundResult {
		t.Errorf("parent missing subagent_result: events=%v", kinds(parentEvents))
	}
}

// TestRecover_TerminalSubagent_NoOp verifies that a subagent already
// carrying session_terminated is NOT re-terminated and no extra
// subagent_result is written to the parent.
func TestRecover_TerminalSubagent_NoOp(t *testing.T) {
	store := newFakeStore()
	ctx := context.Background()

	now := time.Now().UTC()
	_ = store.OpenSession(ctx, SessionRow{
		ID: "root1", AgentID: "a1", SessionType: "root", Status: StatusActive,
		CreatedAt: now, UpdatedAt: now,
	})
	_ = store.OpenSession(ctx, SessionRow{
		ID: "sub1", AgentID: "a1", ParentSessionID: "root1", SessionType: "subagent", Status: StatusActive,
		Metadata:  map[string]any{"depth": 1},
		CreatedAt: now, UpdatedAt: now,
	})
	// Pre-seed the subagent as terminal with a different reason.
	terminal := protocol.NewSessionTerminated("sub1", protocol.ParticipantInfo{ID: "a1", Kind: protocol.ParticipantAgent},
		protocol.SessionTerminatedPayload{Reason: protocol.TerminationCompleted})
	row, summary, _ := FrameToEventRow(terminal, "a1")
	_ = store.AppendEvent(ctx, row, summary)

	mgr := newTestManager(t, store)
	if _, err := Recover(ctx, mgr.deps); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	subEvents, _ := store.ListEvents(ctx, "sub1", ListEventsOpts{})
	termCount := 0
	for _, ev := range subEvents {
		if ev.EventType == string(protocol.KindSessionTerminated) {
			termCount++
		}
	}
	if termCount != 1 {
		t.Errorf("subagent has %d session_terminated events, want 1", termCount)
	}

	parentEvents, _ := store.ListEvents(ctx, "root1", ListEventsOpts{})
	for _, ev := range parentEvents {
		if ev.EventType == string(protocol.KindSubagentResult) {
			t.Errorf("parent unexpectedly received subagent_result for already-terminal child")
		}
	}
}

// TestRestoreActive_ResumesNonTerminalRoots verifies the boot loop
// resumes only non-terminal root sessions and skips subagents.
func TestRestoreActive_ResumesNonTerminalRoots(t *testing.T) {
	store := newFakeStore()
	ctx := context.Background()
	now := time.Now().UTC()

	_ = store.OpenSession(ctx, SessionRow{
		ID: "root_live", AgentID: "a1", SessionType: "root", Status: StatusActive,
		CreatedAt: now, UpdatedAt: now,
	})
	_ = store.OpenSession(ctx, SessionRow{
		ID: "root_dead", AgentID: "a1", SessionType: "root", Status: StatusActive,
		CreatedAt: now, UpdatedAt: now,
	})
	// Seed root_dead with a terminal event so it's skipped.
	dead := protocol.NewSessionTerminated("root_dead",
		protocol.ParticipantInfo{ID: "a1", Kind: protocol.ParticipantAgent},
		protocol.SessionTerminatedPayload{Reason: protocol.TerminationUserEnd})
	dRow, dSum, _ := FrameToEventRow(dead, "a1")
	_ = store.AppendEvent(ctx, dRow, dSum)

	mgr := newTestManager(t, store)
	if err := mgr.RestoreActive(ctx); err != nil {
		t.Fatalf("RestoreActive: %v", err)
	}
	defer mgr.ShutdownAll(ctx)

	live := mgr.SessionsLive()
	wantLive := map[string]bool{"root_live": true}
	if len(live) != len(wantLive) {
		t.Fatalf("live = %v, want only root_live", live)
	}
	for _, id := range live {
		if !wantLive[id] {
			t.Errorf("unexpected live session %q", id)
		}
	}
}

// TestRestoreActive_AutoRespawnsDanglingSubagent verifies the phase-4
// US6 contract: a non-terminal sub-agent observed at boot is settled
// with restart_died (existing Recover semantics) AND a fresh sub-agent
// is spawned by the runtime under the same root with the original
// (skill, role, task, inputs); the parent's events get a
// system_message{kind:"spawned_note"} narrating the maneuver.
func TestRestoreActive_AutoRespawnsDanglingSubagent(t *testing.T) {
	store := newFakeStore()
	ctx := context.Background()
	now := time.Now().UTC()

	rootID := "root-r1"
	oldChildID := "sub-old1"

	// Seed root + (non-terminal) sub-agent rows.
	if err := store.OpenSession(ctx, SessionRow{
		ID: rootID, AgentID: "a1", SessionType: "root", Status: StatusActive,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("OpenSession root: %v", err)
	}
	if err := store.OpenSession(ctx, SessionRow{
		ID: oldChildID, AgentID: "a1", ParentSessionID: rootID,
		SessionType: "subagent", Status: StatusActive,
		Metadata:    map[string]any{"depth": 1},
		CreatedAt:   now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("OpenSession subagent: %v", err)
	}

	// Seed the parent's subagent_started event with the original spec
	// — auto-respawn reads this back to drive parent.Spawn(spec).
	started := protocol.NewSubagentStarted(rootID,
		protocol.ParticipantInfo{ID: "a1", Kind: protocol.ParticipantAgent},
		protocol.SubagentStartedPayload{
			ChildSessionID: oldChildID,
			Skill:          "demo",
			Role:           "explorer",
			Task:           "explore the catalog",
			Depth:          1,
			StartedAt:      now,
		})
	sRow, sSum, err := FrameToEventRow(started, "a1")
	if err != nil {
		t.Fatalf("FrameToEventRow subagent_started: %v", err)
	}
	if err := store.AppendEvent(ctx, sRow, sSum); err != nil {
		t.Fatalf("AppendEvent subagent_started: %v", err)
	}

	mgr := newTestManager(t, store)
	if err := mgr.RestoreActive(ctx); err != nil {
		t.Fatalf("RestoreActive: %v", err)
	}
	defer mgr.ShutdownAll(ctx)

	// 1. Old sub-agent has session_terminated{restart_died}.
	oldEvents, _ := store.ListEvents(ctx, oldChildID, ListEventsOpts{})
	if !containsKindWithReason(oldEvents, protocol.KindSessionTerminated, protocol.TerminationRestartDied) {
		t.Fatalf("old sub-agent missing session_terminated{restart_died}; events=%v",
			kinds(oldEvents))
	}

	// 2. Parent has the synthetic subagent_result for the old child.
	parentEvents, _ := store.ListEvents(ctx, rootID, ListEventsOpts{})
	var foundOldResult bool
	for _, ev := range parentEvents {
		if ev.EventType != string(protocol.KindSubagentResult) {
			continue
		}
		if ev.Metadata["session_id"] == oldChildID &&
			ev.Metadata["reason"] == protocol.TerminationRestartDied {
			foundOldResult = true
			break
		}
	}
	if !foundOldResult {
		t.Fatalf("parent missing synthetic subagent_result{old_child, restart_died}; events=%v",
			kinds(parentEvents))
	}

	// 3. Parent has a NEW subagent_started naming a different child id
	//    with the same task.
	var newChildID, newTask string
	for _, ev := range parentEvents {
		if ev.EventType != string(protocol.KindSubagentStarted) {
			continue
		}
		cid, _ := ev.Metadata["child_session_id"].(string)
		if cid == "" || cid == oldChildID {
			continue
		}
		newChildID = cid
		newTask, _ = ev.Metadata["task"].(string)
		break
	}
	if newChildID == "" {
		t.Fatalf("no fresh subagent_started observed on parent; events=%v",
			kinds(parentEvents))
	}
	if newTask != "explore the catalog" {
		t.Errorf("respawn task = %q, want %q", newTask, "explore the catalog")
	}

	// 4. Parent has a system_message{kind:"spawned_note"} mentioning the
	//    maneuver — old id + new id both present in the body.
	var foundSpawnedNote bool
	for _, ev := range parentEvents {
		if ev.EventType != string(protocol.KindSystemMessage) {
			continue
		}
		k, _ := ev.Metadata["kind"].(string)
		if k != protocol.SystemMessageSpawnedNote {
			continue
		}
		if !strings.Contains(ev.Content, oldChildID) ||
			!strings.Contains(ev.Content, newChildID) {
			continue
		}
		foundSpawnedNote = true
		break
	}
	if !foundSpawnedNote {
		t.Errorf("parent missing system_message{spawned_note} bridging old=%s new=%s; events=%v",
			oldChildID, newChildID, kinds(parentEvents))
	}

	// 5. The new sub-agent session row exists in the store.
	if _, err := store.LoadSession(ctx, newChildID); err != nil {
		t.Errorf("new sub-agent row missing from store: %v", err)
	}
}

func containsKindWithReason(events []EventRow, kind protocol.Kind, reason string) bool {
	for _, ev := range events {
		if ev.EventType == string(kind) {
			if r, _ := ev.Metadata["reason"].(string); r == reason {
				return true
			}
		}
	}
	return false
}

func kinds(events []EventRow) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.EventType)
	}
	return out
}
