package session

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// asyncSummaryPrompt is the synthetic user-role prompt the runtime
// stamps onto the session when one or more async missions complete
// and root has no in-flight turn to consume their
// [system:subagent_result] inject. The model sees both the inject(s)
// (folded into s.history just above this synthetic message) and the
// directive below, and surfaces the result to the user without
// requiring them to type a follow-up first.
//
// Wording kept terse — the inject already carries the mission goal +
// rendered result via `interrupts/async_mission_completed.tmpl`.
const asyncSummaryPrompt = "[system:async_summary] One or more async missions completed since the previous user message. " +
	"The [system:async_mission_completed] inject(s) above carry their results. " +
	"Write the user-facing reply now — summarise what was found and what to do next. " +
	"Do NOT re-spawn the same mission. " +
	"Do NOT call session:notify_subagent for a completed mission — it has already terminated; the result is in the inject above. " +
	"If the result is incomplete or ambiguous, ask the user a clarifying question instead of looping on tool calls."

// kickAsyncSummaryTurn starts a system-initiated turn driven by a
// synthetic UserMessage authored by the agent participant. Phase
// 5.1c.cancel-ux follow-up — the model sees the just-folded
// [system:subagent_result] inject(s) in its history and produces a
// user-facing reply summarising the mission(s) outcome.
//
// Safe to call when:
//   - The session is not closed / closing.
//   - There is no in-flight turn (turnState == nil).
//
// When a turn is already in flight the caller is expected to re-arm
// pendingAsyncSummary so the end-of-turn boundary fires the kick.
// The TUI suppresses inbound UserMessage frames (they are echoed on
// submit), so the synthetic prompt is invisible to the operator.
//
// The synthetic message IS persisted to the event log — replay /
// restart will see the same system-initiated trigger and reproduce
// the same turn. Author=agent.Participant() distinguishes it from
// real user input.
func (s *Session) kickAsyncSummaryTurn(ctx context.Context) {
	if s == nil || s.IsClosed() || s.IsClosing() {
		return
	}
	if s.turnState != nil {
		// Another turn started between the caller's idle check and
		// this entry — re-arm so the end-of-turn boundary handles
		// it. Avoids re-entering startTurn while one is already in
		// flight.
		s.pendingAsyncSummary.Store(true)
		return
	}
	synth := protocol.NewUserMessage(s.id, s.agent.Participant(), asyncSummaryPrompt)
	s.startTurn(ctx, synth)
}
