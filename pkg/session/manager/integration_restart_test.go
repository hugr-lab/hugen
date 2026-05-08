package manager

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
)

// integration_restart_test.go covers the phase-4 acceptance scenarios
// §13.2 #19 (restart resume — adapted to the new settle-based design,
// commit US6) and §13.2 #20b (graceful shutdown writes nothing). These
// are end-to-end checks across the Manager lifecycle: open + spawn,
// graceful Stop, fresh Manager pointed at the same store,
// RestoreActive — observable contract surfaced through the persisted
// events log.
//
// The scenarios are adapted from the original spec text where it
// described top-down BFS restart. The settle ADR
// (`design/001-agent-runtime/phase-4-restart-settle.md`) replaced that
// with: only roots restore eagerly; only direct children of restored
// roots get settled; the model decides whether to re-spawn. These
// tests pin that contract.

// TestPhase4Acceptance_GracefulShutdownWritesNothing — §13.2 #20b.
//
// Spawn root + sub (one direct child). Call Stop(ctx) — the
// graceful path: rootCancel fires with no terminationCause attached,
// each session's Run goroutine returns from teardown(runCtx) without
// persisting anything. We verify both event logs are clean of any
// session_terminated row.
//
// Then on the next "boot" — a fresh Manager pointed at the same
// store — RestoreActive runs settleDanglingSubagents on the root.
// The dangling sub gets session_terminated{restart_died} and the root
// gets a synthetic subagent_result{restart_died}. Idle/active filter
// (D5 of the settle ADR) classifies the root as active because settle
// wrote a row, so its goroutine comes back up.
//
// This is the load-bearing safety check on the settle design: a clean
// shutdown doesn't lie about state — sessions stay non-terminal in
// the DB until the next boot reconciles them.
func TestPhase4Acceptance_GracefulShutdownWritesNothing(t *testing.T) {
	store := fixture.NewTestStore()
	ctx := context.Background()

	// --- First Manager: open root + spawn sub ---
	mgr1 := newTestManager(t, store)
	root, _, err := mgr1.Open(ctx, session.OpenRequest{OwnerID: "alice"})
	if err != nil {
		t.Fatalf("Open root: %v", err)
	}
	rootID := root.ID()
	drainOutboxOnce(root.Outbox()) // SessionOpened

	sub, err := root.Spawn(ctx, session.SpawnSpec{
		Skill: "demo",
		Role:  "worker",
		Task:  "do the thing",
	})
	if err != nil {
		t.Fatalf("Spawn sub: %v", err)
	}
	subID := sub.ID()
	drainOutboxOnce(root.Outbox()) // SubagentStarted

	// --- Graceful shutdown: rootCancel without cause ---
	mgr1.Stop(ctx)

	// Verify: NEITHER session has a session_terminated event yet.
	rootEvents, _ := store.ListEvents(ctx, rootID, session.ListEventsOpts{})
	for _, ev := range rootEvents {
		if ev.EventType == string(protocol.KindSessionTerminated) {
			t.Errorf("graceful shutdown wrote session_terminated on root: %v", ev)
		}
	}
	subEvents, _ := store.ListEvents(ctx, subID, session.ListEventsOpts{})
	for _, ev := range subEvents {
		if ev.EventType == string(protocol.KindSessionTerminated) {
			t.Errorf("graceful shutdown wrote session_terminated on sub: %v", ev)
		}
	}

	// --- Second Manager (next "boot"): RestoreActive ---
	mgr2 := newTestManager(t, store)
	if err := mgr2.RestoreActive(ctx); err != nil {
		t.Fatalf("RestoreActive: %v", err)
	}
	defer mgr2.Stop(ctx)

	// Settle wrote restart_died on the sub.
	subEvents2, _ := store.ListEvents(ctx, subID, session.ListEventsOpts{})
	if !containsKindWithReason(subEvents2,
		protocol.KindSessionTerminated, protocol.TerminationRestartDied) {
		t.Errorf("post-boot: sub missing session_terminated{restart_died}; events=%v",
			eventKinds(subEvents2))
	}

	// Settle wrote a synthetic subagent_result{restart_died} on root.
	rootEvents2, _ := store.ListEvents(ctx, rootID, session.ListEventsOpts{})
	var sawResult bool
	for _, ev := range rootEvents2 {
		if ev.EventType != string(protocol.KindSubagentResult) {
			continue
		}
		if ev.Metadata["session_id"] != subID {
			continue
		}
		if ev.Metadata["reason"] == protocol.TerminationRestartDied {
			sawResult = true
			break
		}
	}
	if !sawResult {
		t.Errorf("post-boot: root missing subagent_result{%s, restart_died}; events=%v",
			subID, eventKinds(rootEvents2))
	}

	// Active root — goroutine eagerly restored by RestoreActive because
	// settle wrote a row. Verifies the active/idle filter (D5).
	live := mgr2.SessionsLive()
	var seen bool
	for _, id := range live {
		if id == rootID {
			seen = true
			break
		}
	}
	if !seen {
		t.Errorf("post-boot: root not in SessionsLive (D5 active filter broken): live=%v", live)
	}
}

// TestPhase4Acceptance_RestartResume_TwoSiblings — §13.2 #19, adapted.
//
// Spec text described a 2-deep tree (root → sub → sub-sub) and "both
// subagent_result Frames" surfacing on root. The settle ADR (D7)
// replaced top-down BFS with "only roots restore; only direct
// children get settled" — so the realistic phase-4 shape is a root
// with multiple direct children. This test exercises that with two
// siblings; both must surface restart_died on the parent.
//
// Concretely: root → sub-A + sub-B; Stop graceful; new
// Manager + RestoreActive. After settle, root.events must contain a
// subagent_result for EACH of sub-A and sub-B with reason
// restart_died, and EACH sub's events must carry session_terminated
// {restart_died}. The model on the next user message will see both
// results in s.history (via the projectHistory subagent_result
// rendering — verified separately in TestProjectHistory_IncludesSubagentFrames).
func TestPhase4Acceptance_RestartResume_TwoSiblings(t *testing.T) {
	store := fixture.NewTestStore()
	ctx := context.Background()

	mgr1 := newTestManager(t, store)
	root, _, err := mgr1.Open(ctx, session.OpenRequest{OwnerID: "alice"})
	if err != nil {
		t.Fatalf("Open root: %v", err)
	}
	rootID := root.ID()
	drainOutboxOnce(root.Outbox())

	subA, err := root.Spawn(ctx, session.SpawnSpec{Role: "explorer", Task: "scout-a"})
	if err != nil {
		t.Fatalf("Spawn subA: %v", err)
	}
	drainOutboxOnce(root.Outbox())
	subB, err := root.Spawn(ctx, session.SpawnSpec{Role: "explorer", Task: "scout-b"})
	if err != nil {
		t.Fatalf("Spawn subB: %v", err)
	}
	drainOutboxOnce(root.Outbox())

	// Graceful shutdown — siblings still running, no terminations
	// written.
	mgr1.Stop(ctx)

	// --- Boot 2 ---
	mgr2 := newTestManager(t, store)
	if err := mgr2.RestoreActive(ctx); err != nil {
		t.Fatalf("RestoreActive: %v", err)
	}
	defer mgr2.Stop(ctx)

	// Each sub has session_terminated{restart_died}.
	for _, sub := range []*session.Session{subA, subB} {
		evs, _ := store.ListEvents(ctx, sub.ID(), session.ListEventsOpts{})
		if !containsKindWithReason(evs,
			protocol.KindSessionTerminated, protocol.TerminationRestartDied) {
			t.Errorf("sub %s missing session_terminated{restart_died}; events=%v",
				sub.ID(), eventKinds(evs))
		}
	}

	// Root has subagent_result{restart_died} for BOTH children.
	rootEvents, _ := store.ListEvents(ctx, rootID, session.ListEventsOpts{})
	results := map[string]string{}
	for _, ev := range rootEvents {
		if ev.EventType != string(protocol.KindSubagentResult) {
			continue
		}
		cid, _ := ev.Metadata["session_id"].(string)
		reason, _ := ev.Metadata["reason"].(string)
		if cid != "" {
			results[cid] = reason
		}
	}
	for _, sub := range []*session.Session{subA, subB} {
		got, ok := results[sub.ID()]
		if !ok {
			t.Errorf("root missing subagent_result for child %s; events=%v",
				sub.ID(), eventKinds(rootEvents))
			continue
		}
		if got != protocol.TerminationRestartDied {
			t.Errorf("subagent_result for %s reason = %q, want %s",
				sub.ID(), got, protocol.TerminationRestartDied)
		}
	}

	// Sanity: the synthetic result body carries the child id and the
	// reason — that's the narrative line projectHistory will render
	// into the model's history on next materialise.
	for _, ev := range rootEvents {
		if ev.EventType != string(protocol.KindSubagentResult) {
			continue
		}
		cid, _ := ev.Metadata["session_id"].(string)
		body := ev.Content
		if body == "" {
			body, _ = ev.Metadata["result"].(string)
		}
		if cid != "" && !strings.Contains(body, cid) {
			t.Errorf("subagent_result body for %s does not mention id; body=%q",
				cid, body)
		}
	}

	// Root is alive after restore (active filter D5 — settle wrote
	// non-zero rows).
	live := mgr2.SessionsLive()
	var seenRoot bool
	for _, id := range live {
		if id == rootID {
			seenRoot = true
		}
	}
	if !seenRoot {
		t.Errorf("root with 2 dangling subs not in SessionsLive: live=%v", live)
	}
}

// eventKinds is a local shortcut for diagnostics in this file. The
// global `kinds` helper used to live in recover_test.go but is
// unused in the settle-only suite — reintroducing a private helper
// keeps the dependency tight.
func eventKinds(events []session.EventRow) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.EventType)
	}
	return out
}

// time import retention in case future scenarios add timeouts.
var _ = time.Second
