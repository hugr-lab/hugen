package session

import (
	"context"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// TestRouteInbound_AsyncSubagentResult_ArmsFlag — when a
// SubagentResult arrives with RenderMode=async_notify, the
// pendingAsyncSummary flag is armed so end-of-turn / idle-route
// paths can fire the auto-summary kick. Phase 5.1c.cancel-ux
// follow-up.
func TestRouteInbound_AsyncSubagentResult_ArmsFlag(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()
	// Mark closing so the idle-route auto-summary kick is suppressed
	// and the swap does not clear the flag — leaves the armed state
	// observable for assertion.
	parent.markClosing()

	sr := protocol.NewSubagentResult(parent.ID(), "child-abc", parent.agent.Participant(),
		protocol.SubagentResultPayload{
			SessionID:  "child-abc",
			Reason:     protocol.TerminationCompleted,
			Result:     "summary text",
			Goal:       "goal text",
			RenderMode: protocol.SubagentRenderAsyncNotify,
		})
	if err := parent.routeInbound(context.Background(), sr); err != nil {
		t.Fatalf("routeInbound: %v", err)
	}
	if !parent.pendingAsyncSummary.Load() {
		t.Errorf("pendingAsyncSummary not armed after async_notify SubagentResult")
	}
}

// TestRouteInbound_AsyncSubagentResult_NonAsyncMode_NoFlag —
// SubagentResult with empty / silent RenderMode must NOT arm the
// summary flag. Only async-notify async-completed missions trigger
// the proactive surface.
func TestRouteInbound_AsyncSubagentResult_NonAsyncMode_NoFlag(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	sr := protocol.NewSubagentResult(parent.ID(), "child-abc", parent.agent.Participant(),
		protocol.SubagentResultPayload{
			SessionID:  "child-abc",
			Reason:     protocol.TerminationCompleted,
			Result:     "summary text",
			Goal:       "goal text",
			RenderMode: "",
		})
	if err := parent.routeInbound(context.Background(), sr); err != nil {
		t.Fatalf("routeInbound: %v", err)
	}
	if parent.pendingAsyncSummary.Load() {
		t.Errorf("pendingAsyncSummary armed for non-async render mode")
	}
}

// TestRouteInbound_IdleFold_PopulatesHistory — when an async
// SubagentResult arrives at an idle session, the frame is folded
// into s.history via projectFrameToHistory so the next turn's
// prompt build (buildMessages → s.history) carries the
// [system:subagent_result] inject. Without this fold the model
// loses the result between turns and resorts to notify_subagent
// calls against the now-closed child. Phase 5.1c.cancel-ux
// follow-up — operator dogfood fix.
func TestRouteInbound_IdleFold_PopulatesHistory(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()
	// Suppress the auto-summary kick so we can isolate the history-
	// fold step. The kick would synthesise a UserMessage and append
	// it to history too, making the +1 assertion fail with +2.
	parent.markClosing()
	historyBefore := len(parent.history)

	sr := protocol.NewSubagentResult(parent.ID(), "child-abc", parent.agent.Participant(),
		protocol.SubagentResultPayload{
			SessionID:  "child-abc",
			Reason:     protocol.TerminationCompleted,
			Result:     "the mission found three tables",
			Goal:       "list payment-related tables",
			RenderMode: protocol.SubagentRenderAsyncNotify,
		})
	if err := parent.routeInbound(context.Background(), sr); err != nil {
		t.Fatalf("routeInbound: %v", err)
	}
	if got := len(parent.history); got != historyBefore+1 {
		t.Fatalf("history grew by %d; want +1", got-historyBefore)
	}
	last := parent.history[len(parent.history)-1]
	if last.Role == "" {
		t.Errorf("folded history entry has empty role")
	}
	// projectFrameToHistory renders SubagentResult{AsyncNotify} as a
	// RoleUser message carrying the `interrupts/async_mission_completed`
	// template output; the goal + result must show through.
	if !strings.Contains(last.Content, "list payment-related tables") {
		t.Errorf("history inject missing goal text; got %q", last.Content)
	}
	if !strings.Contains(last.Content, "the mission found three tables") {
		t.Errorf("history inject missing result summary; got %q", last.Content)
	}
}

// TestRouteInbound_IdleAsyncResult_KicksSummaryTurn — when an
// async-notify SubagentResult arrives at an idle root session,
// routeInbound's RouteBuffered idle branch fires the auto-summary
// kick: it folds the inject into history, emits, and synthesises a
// UserMessage that starts a new turn. turnState becomes non-nil
// after the call. The synthetic message is authored by the agent
// participant so the TUI's UserMessage handler suppresses it from
// the chat.
func TestRouteInbound_IdleAsyncResult_KicksSummaryTurn(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	sr := protocol.NewSubagentResult(parent.ID(), "child-abc", parent.agent.Participant(),
		protocol.SubagentResultPayload{
			SessionID:  "child-abc",
			Reason:     protocol.TerminationCompleted,
			Result:     "found three tables",
			Goal:       "list payment tables",
			RenderMode: protocol.SubagentRenderAsyncNotify,
		})
	if err := parent.routeInbound(context.Background(), sr); err != nil {
		t.Fatalf("routeInbound: %v", err)
	}
	if parent.turnState == nil {
		t.Fatalf("auto-summary kick did not start a turn")
	}
	if parent.pendingAsyncSummary.Load() {
		t.Errorf("flag should be cleared after the kick")
	}
	// Cleanup: cancel the kicked turn so the test exits clean.
	if parent.turnCancel != nil {
		parent.turnCancel()
	}
	parent.turnWG.Wait()
	parent.retireTurn()
}

// TestStartTurn_ClearsPendingAsyncSummary — a user-driven turn
// supersedes a queued auto-summary kick: the new turn's history
// already carries the inject (via materialise or prior fold) and
// the model will see it on its own. Avoids a duplicate summary
// turn firing immediately after the user's turn ends.
func TestStartTurn_ClearsPendingAsyncSummary(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	parent.pendingAsyncSummary.Store(true)
	user := protocol.ParticipantInfo{
		ID:   parent.ownerID,
		Kind: protocol.ParticipantUser,
		Name: parent.ownerID,
	}
	um := protocol.NewUserMessage(parent.ID(), user, "hi")
	parent.startTurn(context.Background(), um)
	if parent.pendingAsyncSummary.Load() {
		t.Errorf("startTurn did not clear pendingAsyncSummary")
	}
	// Cleanup: cancel the just-started turn so test exit is clean.
	if parent.turnCancel != nil {
		parent.turnCancel()
	}
	parent.turnWG.Wait()
	parent.retireTurn()
}

// TestKickAsyncSummaryTurn_NoopWhenClosed — calling the kicker on
// a closed / closing session is a safe no-op; the operator may
// have hit /end concurrently with an async-completion landing.
func TestKickAsyncSummaryTurn_NoopWhenClosed(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	parent.markClosing()
	parent.kickAsyncSummaryTurn(context.Background())
	if parent.turnState != nil {
		t.Errorf("kicker started a turn against a closing session")
	}
}

// TestKickAsyncSummaryTurn_RearmsWhenBusy — the kicker should not
// re-enter startTurn while another turn is in flight; it re-arms
// the flag so the end-of-turn boundary picks it up instead.
func TestKickAsyncSummaryTurn_RearmsWhenBusy(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	// Fake an in-flight turn by setting turnState directly. Real
	// startTurn machinery is exercised by other tests; here we only
	// care that the kicker's busy-check works.
	parent.turnState = &turnState{}
	defer func() { parent.turnState = nil }()

	parent.kickAsyncSummaryTurn(context.Background())
	if !parent.pendingAsyncSummary.Load() {
		t.Errorf("kicker should re-arm pendingAsyncSummary when busy")
	}
}

// TestRouteInbound_BusyPath_QueuesPendingInbound — when a turn is
// in flight, an async SubagentResult goes to pendingInbound (will
// be folded at the next turn boundary via drainPendingInbound).
// The flag is still armed so end-of-turn can kick the summary.
func TestRouteInbound_BusyPath_QueuesPendingInbound(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	parent.turnState = &turnState{}
	defer func() {
		parent.turnState = nil
		parent.pendingInbound = nil
	}()

	pendingBefore := len(parent.pendingInbound)
	sr := protocol.NewSubagentResult(parent.ID(), "child-abc", parent.agent.Participant(),
		protocol.SubagentResultPayload{
			SessionID:  "child-abc",
			Reason:     protocol.TerminationCompleted,
			RenderMode: protocol.SubagentRenderAsyncNotify,
		})
	if err := parent.routeInbound(context.Background(), sr); err != nil {
		t.Fatalf("routeInbound: %v", err)
	}
	if got := len(parent.pendingInbound); got != pendingBefore+1 {
		t.Errorf("pendingInbound grew by %d; want +1", got-pendingBefore)
	}
	if !parent.pendingAsyncSummary.Load() {
		t.Errorf("pendingAsyncSummary not armed on busy path")
	}
}

