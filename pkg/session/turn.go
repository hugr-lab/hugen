package session

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// turnState carries the runtime accumulator for one user-message-driven
// model↔tools loop. Lifetime: created in startTurn, advanced through
// handleModelEvent / handleToolResult, retired in advanceOrFinish or
// rollbackTurn. Single-goroutine ownership (Session.Run) — no locks.
type turnState struct {
	// seqBaseline is the per-session seq of the user message that
	// started this turn (assigned by emit). Roll-back paths trim
	// the compactor-owned history cache to entries with Seq ≤
	// baseline so any consolidated assistant / tool_result rows
	// the cancelled turn emitted disappear from the model's
	// next view. The user message itself is preserved — η spec
	// §6 captures the cancel-semantics rationale.
	seqBaseline int64

	// iter is the current model→tools→model iteration index (0-based).
	// cap is the per-turn soft ceiling resolved once at startTurn
	// from resolveToolIterCap; capHard is the matching hard ceiling
	// from resolveHardCeiling. Sampled together so the soft / hard
	// pair stays coherent through the loop even if a tool call
	// mutates the loaded skills mid-turn.
	iter    int
	cap     int
	capHard int

	// mdl is the resolved Model bound for this turn. Cached so iter
	// loops don't re-resolve (Resolve is cheap but emitting a fresh
	// system_marker on resolve-time errors is wrong mid-turn).
	mdl model.Model

	// Per-iteration accumulator drained from modelChunks. Reset at the
	// top of each iteration via resetModelAccumulator.
	finalText        string
	toolCalls        []model.ChunkToolCall
	thinking         string
	thoughtSignature string
	sawFinal         bool
	agentSeq         int
	reasoningSeq     int

	// turnUsage aggregates each iter's model.Usage over the turn.
	// Stamped on the Final=true Consolidated AgentMessage so the
	// outbox carries the turn's total cost. NOT reset between
	// iters — it's per-turn, not per-iter. Folded into
	// Session.cumulativeUsage at the same emit. Phase 5.2
	// (context-budget observability).
	turnUsage protocol.TokenUsage

	// Tool-result tracking for the current iteration. Pending = call
	// dispatched but result not yet seen on toolResults; len==0 means
	// "all tool_results matched" and the loop can build the next prompt.
	pendingToolCalls map[string]model.ChunkToolCall

	// streamErr captures the most recent model.Generate / stream.Next
	// failure for advanceOrFinish to surface as an Error frame after
	// the model channel drains.
	streamErr error

	// assistantFolded marks "the assistant message for this iteration
	// has been appended to the owned history cache". advanceOrFinish runs at every
	// turn boundary; without this flag a re-entry after the tool
	// dispatcher exits would re-fold the same assistant message and
	// re-dispatch the same tool calls in a tight loop. Reset to false
	// at the top of each iteration via resetModelAccumulator.
	assistantFolded bool

	// finalizeGateRetries counts how many times a
	// [extension.TurnFinalizeGate] has vetoed this turn's
	// finalization and the runtime re-iterated the same session
	// instead of retiring. Bounded by maxFinalizeGateRetries — past
	// the cap the gate is skipped and the turn retires regardless, so
	// a gate that never allows (e.g. a planner that never produces an
	// approved plan) can't loop forever. NOT reset per iteration —
	// it's a per-turn backstop.
	finalizeGateRetries int
}

// modelChunkEvent is the single union the Run loop reads from
// s.modelChunks. done=true is the stream's terminal sentinel — chunk
// is zero, err may be non-nil if the stream failed mid-flight.
type modelChunkEvent struct {
	chunk model.Chunk
	done  bool
	err   error
}

// toolResultEvent is the single union the Run loop reads from
// s.toolResults. payload is the JSON string fed back to the model as
// the tool-role message; "" on dispatch failure (the tool_error
// Frame already landed in the transcript).
type toolResultEvent struct {
	callID  string
	payload string
	// errored is true when the dispatch path surfaced a tool_error
	// frame (permission deny, not_found, io failure, provider returned
	// error). Routed through the event so the Run goroutine can update
	// stuck-detection state without racing the dispatcher.
	errored bool
}

// ToolFeed reserves the slot a phase-4 blocking system tool (the
// canonical example is wait_subagents) uses to consume Frames the
// session would otherwise buffer. Tools register a feed via
// [Session.registerToolFeed] for the duration of the block and
// release it on return.
type ToolFeed struct {
	// Consumes returns true for Frames the active tool wants to
	// receive. The Run loop checks this before falling back to
	// pendingInbound. Phase-5.1 § β widens the predicate from a
	// Kind-only check to a full-Frame inspection so callers can
	// match on Frame metadata (FromSession for parent notes,
	// RequestID for inquiry responses, etc.) — not just kind.
	Consumes func(protocol.Frame) bool
	// Feed delivers a matching Frame to the tool's blocking handler.
	// Must be non-blocking (the loop is single-goroutine).
	Feed func(protocol.Frame)
	// BlockingState is the [protocol.SessionStatus] state value the
	// session should transition to while this feed is registered.
	// Empty string skips the lifecycle transition (the feed is
	// "active" but not separately observable). Today
	// callWaitSubagents fills in SessionStatusWaitSubagents; the
	// phase-5 HITL approval / clarification tools will fill
	// SessionStatusWaitApproval / SessionStatusWaitUserInput.
	BlockingState string
	// BlockingReason is the diagnostic label the lifecycle marker
	// records alongside the state transition. Free-form, never
	// branched on.
	BlockingReason string
}

// maxFinalizeGateRetries bounds how many times a
// [extension.TurnFinalizeGate] may veto a single turn's finalization
// before the runtime retires the turn regardless. The backstop keeps
// a gate that never allows (e.g. a planner that keeps producing an
// invalid / unapproved plan) from looping forever — past the cap the
// turn ends and the mission's own planner-loop error path takes over.
// 6 leaves a weak model ample room to fix a validate_and_approve body
// (the dogfood failures cleared within 2-3 corrections) without
// burning unbounded model calls.
const maxFinalizeGateRetries = 6

// startTurn moves the Session from idle to "model goroutine running".
// Called from the inbound branch of Run when a UserMessage arrives and
// no prior turn is in flight. After this returns the loop expects to
// drive everything else through s.modelChunks / s.toolResults.
//
//   - The user message is persisted via emit (transcript-visible).
//   - Lazy materialise hydrates extension state if this is the first turn
//     after a process restart.
//   - A pending /model use marker is emitted before the first prompt.
//   - The model is resolved; on failure an Error frame surfaces and
//     the turn does not start (s.turnState stays nil).
//   - turnCtx is forked from runCtx so /cancel can abort model + tools
//     without tearing down the session.
//   - The model goroutine launches; chunks fan in over s.modelChunks.
func (s *Session) startTurn(runCtx context.Context, f *protocol.UserMessage) {
	// Phase 5.1c.cancel-ux follow-up — any starting turn supersedes
	// a queued auto-summary kick: the new turn's history (built
	// below via materialise + drainPendingInbound at boundaries)
	// already carries the SubagentResult inject(s), and the model
	// will see them naturally. Clearing here avoids a duplicate
	// summary turn firing immediately after this one ends.
	s.pendingAsyncSummary.Store(false)
	// Lifecycle: leaving idle for active. Emit BEFORE persisting the
	// user message so a restart that crashes between the active
	// markStatus and the user_message emit still observes the
	// session as active and classifies it eagerly.
	s.markStatus(runCtx, protocol.SessionStatusActive, "user_message")
	if err := s.emit(runCtx, f); err != nil {
		s.logger.Debug("startTurn: emit user", "session", s.id, "err", err)
		return
	}
	if err := s.materialise(runCtx); err != nil {
		s.logger.Warn("materialise failed; proceeding with empty history",
			"session", s.id, "err", err)
	}
	// TurnBoundaryHook: per phase 5.2 §4.1, every implementing
	// extension gets a synchronous callback at the idle→active
	// boundary, AFTER materialise + AFTER the user message
	// emitted so FrameObserver-maintained projections (e.g. the
	// compactor's boundary index) reflect the just-arrived
	// frame. Errors are logged warn-not-fatal — the next
	// boundary retries; the new turn proceeds regardless.
	s.runTurnBoundaryHooks(runCtx)
	if err := s.emitPendingSwitch(runCtx); err != nil {
		s.logger.Debug("startTurn: emit pending switch", "session", s.id, "err", err)
		return
	}
	mdl, _, err := s.models.Resolve(runCtx, model.Hint{
		Intent:        s.DefaultIntent(),
		SessionModels: s.sessionModels(),
	})
	if err != nil {
		errFrame := protocol.NewError(s.id, s.agent.Participant(),
			"model_unavailable", err.Error(), true)
		_ = s.emit(runCtx, errFrame)
		return
	}

	turnCtx, turnCancel := context.WithCancel(runCtx)
	s.turnCtx = turnCtx
	s.turnCancel = turnCancel

	softCap := s.resolveToolIterCap(runCtx)
	s.turnState = &turnState{
		seqBaseline:      int64(f.Seq()),
		cap:              softCap,
		capHard:          s.resolveHardCeiling(runCtx, softCap),
		mdl:              mdl,
		pendingToolCalls: map[string]model.ChunkToolCall{},
	}
	s.startModelIteration(runCtx)
}

// startModelIteration kicks off one model.Generate goroutine for the
// current turnState.iter. Called from startTurn (iter=0) and from
// advanceOrFinish after all tool_results from the previous iteration
// have been collected. Clears the per-iteration accumulator first.
func (s *Session) startModelIteration(runCtx context.Context) {
	st := s.turnState
	if st == nil {
		return
	}
	s.resetModelAccumulator()

	modelTools, err := s.modelToolsForSession(runCtx)
	if err != nil {
		s.logger.Warn("session: build tool catalogue", "session", s.id, "err", err)
	}
	req := model.Request{
		Messages: s.buildMessages(runCtx),
		Tools:    modelTools,
	}
	ch := make(chan modelChunkEvent, 16)
	s.modelChunks = ch
	s.turnWG.Add(1)
	go s.runModelGoroutine(s.turnCtx, runCtx, st.mdl, req, ch)
}

// resetModelAccumulator clears the chunk-collecting fields on
// turnState between model iterations so a previous iteration's
// finalText doesn't leak into the next assistant message.
func (s *Session) resetModelAccumulator() {
	st := s.turnState
	if st == nil {
		return
	}
	st.finalText = ""
	st.toolCalls = nil
	st.thinking = ""
	st.thoughtSignature = ""
	st.sawFinal = false
	st.agentSeq = 0
	st.reasoningSeq = 0
	st.streamErr = nil
	st.assistantFolded = false
}

// runModelGoroutine streams a model.Generate response into ch. Closes
// ch on exit (success, stream error, or turnCtx cancellation) so the
// Run loop's select case sees ok=false and nils s.modelChunks. The
// final event before close has done=true; err is set iff the stream
// failed mid-flight.
//
// runCtx is unused here but kept in the signature so future logging /
// metrics that need session-scope ctx don't change the call shape.
func (s *Session) runModelGoroutine(turnCtx, runCtx context.Context, mdl model.Model, req model.Request, ch chan<- modelChunkEvent) {
	_ = runCtx
	defer s.turnWG.Done()
	defer close(ch)
	stream, err := mdl.Generate(turnCtx, req)
	if err != nil {
		select {
		case ch <- modelChunkEvent{done: true, err: err}:
		case <-turnCtx.Done():
		}
		return
	}
	defer func() { _ = stream.Close() }()
	for {
		chunk, more, err := stream.Next(turnCtx)
		if err != nil {
			select {
			case ch <- modelChunkEvent{done: true, err: err}:
			case <-turnCtx.Done():
			}
			return
		}
		if !more {
			select {
			case ch <- modelChunkEvent{done: true}:
			case <-turnCtx.Done():
			}
			return
		}
		select {
		case ch <- modelChunkEvent{chunk: chunk}:
		case <-turnCtx.Done():
			return
		}
	}
}

// handleModelEvent processes one event from the model goroutine. On
// non-terminal chunks emits Reasoning / AgentMessage frames and
// accumulates the assistant turn in turnState. The terminal event
// (done=true) finalises history-append and either ends the turn (no
// tool calls) or hands off to the tool dispatcher.
func (s *Session) handleModelEvent(runCtx context.Context, ev modelChunkEvent) {
	st := s.turnState
	if st == nil {
		return
	}
	if !ev.done {
		s.applyChunk(runCtx, ev.chunk)
		return
	}
	st.streamErr = ev.err
}

// applyChunk emits Reasoning + AgentMessage frames for one streamed
// chunk and folds tool_calls + reasoning state into turnState. Mirrors
// the pre-C5 streamTurn body, minus the loop wrapper.
//
// Streamed chunks always carry Final=false — they're outbox-only for
// live rendering. The single Final=true frame for the turn is emitted
// by foldAssistantAndMaybeDispatch with the assembled text + tool
// calls + reasoning state, and that one IS persisted. See emit().
func (s *Session) applyChunk(runCtx context.Context, chunk model.Chunk) {
	st := s.turnState
	if chunk.Reasoning != nil && *chunk.Reasoning != "" {
		rf := protocol.NewReasoning(s.id, s.agent.Participant(),
			*chunk.Reasoning, st.reasoningSeq, false)
		if err := s.emit(runCtx, rf); err != nil {
			st.streamErr = err
			return
		}
		st.reasoningSeq++
	}
	if chunk.Content != nil && *chunk.Content != "" {
		st.finalText += *chunk.Content
		af := protocol.NewAgentMessage(s.id, s.agent.Participant(),
			*chunk.Content, st.agentSeq, false)
		if err := s.emit(runCtx, af); err != nil {
			st.streamErr = err
			return
		}
		st.agentSeq++
		if chunk.Final {
			st.sawFinal = true
		}
	}
	if chunk.ToolCall != nil {
		st.toolCalls = append(st.toolCalls, *chunk.ToolCall)
	}
	if chunk.Final {
		if chunk.Thinking != "" {
			st.thinking = chunk.Thinking
		}
		if chunk.ThoughtSignature != "" {
			st.thoughtSignature = chunk.ThoughtSignature
		}
		// Phase 5.2 (context-budget observability) — fold the
		// final chunk's Usage block (when the provider populates
		// it) eagerly into BOTH the per-turn accumulator and
		// the session-cumulative counter. Eager fold means a
		// /cancel or stream error AFTER this iter completed
		// still counts those tokens (the provider already
		// billed for them); only iters whose stream was
		// interrupted BEFORE the final chunk landed escape
		// accounting — and there's no Usage record to fold
		// for those anyway.
		//
		// dogfood 2026-05-21 ε.2 follow-up: previously the
		// fold happened at Final=true Consolidated emit only,
		// which dropped multi-iter turns' usage on cancel.
		if chunk.Usage != nil {
			st.turnUsage.PromptTokens += chunk.Usage.PromptTokens
			st.turnUsage.CompletionTokens += chunk.Usage.CompletionTokens
			s.foldSessionUsage(protocol.TokenUsage{
				PromptTokens:     chunk.Usage.PromptTokens,
				CompletionTokens: chunk.Usage.CompletionTokens,
			})
		}
	}
}

// runToolDispatcher dispatches tool_calls sequentially. Each call
// produces exactly one toolResultEvent on ch; the channel is closed
// on dispatcher exit so the Run loop knows the iteration's tool work
// is done. Sequential dispatch preserves the pre-C5 ordering — the
// model sees tool_results in the same order it requested them.
//
// turnCtx aborts mid-dispatch on /cancel; runCtx is passed through
// for emit so transcript frames keep landing even if the user is
// abandoning the turn.
func (s *Session) runToolDispatcher(turnCtx, runCtx context.Context, calls []model.ChunkToolCall, ch chan<- toolResultEvent) {
	defer s.turnWG.Done()
	defer close(ch)
	for _, tc := range calls {
		select {
		case <-turnCtx.Done():
			return
		default:
		}
		result, errored := s.dispatchToolCall(turnCtx, runCtx, tc)
		select {
		case ch <- toolResultEvent{callID: tc.ID, payload: result, errored: errored}:
		case <-turnCtx.Done():
			return
		}
	}
}

// handleToolResult records one tool_result against turnState.pending.
// The dispatcher already emitted the tool_result frame and the
// compactor's FrameObserver folded it into the owned history
// cache — so the next iteration's prompt build sees it. When
// pending empties, the next iteration kicks off automatically
// via advanceOrFinish.
func (s *Session) handleToolResult(runCtx context.Context, ev toolResultEvent) {
	_ = runCtx
	st := s.turnState
	if st == nil {
		return
	}
	if _, ok := st.pendingToolCalls[ev.callID]; !ok {
		// Spurious result (cancelled tool, race). Best-effort drop.
		return
	}
	delete(st.pendingToolCalls, ev.callID)
	// Propagate the error code (if any) to the stuck-detection
	// trailing window. The repeated_error detector clusters samples
	// by (tool, errCode), so we need the actual code — not just a
	// bool — to distinguish "spawn_wave keeps hitting bad_request"
	// from "spawn_wave hit one transient infrastructure error".
	// Run-goroutine-only access; the dispatcher already exited the
	// per-call critical section by the time this event arrives.
	// η.3 — the tool_result frame was already emitted by the
	// dispatcher (session.go:emitToolResult); the compactor's
	// OnFrameEmit folded it into the owned history cache as a
	// RoleTool entry. η.4 — the stuckdetector extension's
	// OnFrameEmit observes the same emit and annotates its
	// trailing window. Nothing further to do here.
}

// turnComplete answers the question the Run loop asks after every
// select branch: should advanceOrFinish run now? True when:
//   - no turn is in progress, OR
//   - the model goroutine has exited (s.modelChunks==nil) AND
//     the tool dispatcher has exited (s.toolResults==nil).
//
// The conjunction is the explicit pre-condition for "build next prompt
// or end turn" — if either side is still streaming, we're mid-flight.
func (s *Session) turnComplete() bool {
	if s.turnState == nil {
		return false
	}
	return s.modelChunks == nil && s.toolResults == nil
}

// advanceOrFinish runs at every turn boundary. The state machine has
// two stable points per iteration:
//
//   - Just after the model goroutine exited (st.assistantFolded ==
//     false): emit the consolidated assistant message (FrameObserver
//     folds it into the owned history cache); if no tool calls,
//     drain inbound + retire; otherwise dispatch tools and stay
//     in-turn.
//   - Just after the tool dispatcher exited (st.assistantFolded ==
//     true): all tool_result frames were emitted by the dispatcher;
//     the compactor's FrameObserver folded each into the owned
//     history cache. Bump iter, drain inbound, kick off the next
//     model iteration, or surface tool_iteration_limit if cap hit.
//
// Plus the failure shortcuts (any iteration):
//
//   - /cancel (turnCtx.Err): roll back history past the seq
//     baseline, retire. handleCancel already wrote the Cancel
//     frame to outbox.
//   - streamErr: roll back, surface stream_error Error frame, retire.
//
// pendingInbound drain order: handled at every turn boundary BEFORE
// the next prompt is built so RouteBuffered frames reach the model's
// next view of the owned history cache. The drain emits each Frame —
// the compactor's FrameObserver applies the visibility allow-list
// (UserMessage, SubagentStarted, SubagentResult, SystemMessage).
func (s *Session) advanceOrFinish(runCtx context.Context) {
	st := s.turnState
	if st == nil {
		return
	}

	// /cancel — turnCtx cancelled. Mark idle if quiescent so the
	// restart classifier won't eagerly resume a session that just
	// idled out of an aborted turn.
	if s.turnCtx != nil && s.turnCtx.Err() != nil {
		s.rollbackTurn()
		s.retireTurn()
		if s.isQuiescent() {
			s.markStatus(runCtx, protocol.SessionStatusIdle, "cancelled")
		}
		return
	}
	// Stream / Generate error — fold a stream_error frame and bail.
	// Same idle-on-quiescent treatment as cancel: the session is no
	// longer working, even though the turn ended on an error.
	if st.streamErr != nil {
		s.rollbackTurn()
		errFrame := protocol.NewError(s.id, s.agent.Participant(),
			"stream_error", st.streamErr.Error(), true)
		_ = s.emit(runCtx, errFrame)
		s.retireTurn()
		if s.isQuiescent() {
			s.markStatus(runCtx, protocol.SessionStatusIdle, "stream_error")
		}
		return
	}

	if !st.assistantFolded {
		s.foldAssistantAndMaybeDispatch(runCtx)
		return
	}

	// Re-entry after the tool dispatcher exited. handleToolResult has
	// already trimmed pendingToolCalls; the compactor folded each
	// emitted tool_result frame into the owned history cache.
	// Advance to the next iteration (or hit the ceiling).
	st.iter++

	// Hard ceiling (phase-4-spec §8.2): terminate the session via the
	// explicit-cancel teardown path. The deferred handleExit writes
	// session_terminated{reason:"hard_ceiling"} and (for sub-agents)
	// surfaces a clean subagent_result to the parent.
	//
	// No lifecycle marker is emitted on this path: the session is
	// terminating, not idling. The session_terminated event is the
	// final state; Manager.RestoreActive's narrow probe sees that
	// terminal row first (it's the newest of the
	// session_terminated|session_status pair) and skips the
	// session, so the persisted "active" marker that immediately
	// precedes session_terminated never reaches the classifier.
	if st.capHard > 0 && st.iter >= st.capHard {
		s.logger.Warn("session: tool re-call hard ceiling hit",
			"session", s.id, "max_hard", st.capHard, "iter", st.iter)
		s.triggerHardCeiling(runCtx)
		s.retireTurn()
		return
	}

	// Drain pendingInbound BEFORE injecting the soft warning / stuck
	// nudges so any runtime-buffered Frames (subagent_result, …) land
	// in the owned history cache first; then layer the local nudges
	// on top.
	s.drainPendingInbound(runCtx)
	// Soft warning (§8.1) — fired exactly once per session when the
	// model crosses st.cap. Subsequent boundaries no-op via softWarningDone.
	s.maybeInjectSoftWarning(runCtx)
	// Stuck-detection rising-edge nudges (§8.3) — η.4 moved the
	// detectors into `pkg/extension/stuckdetector`. The extension
	// observes consolidated AgentMessage + ToolResult frames via
	// FrameObserver and emits nudges synchronously on transition.
	// Nothing to do here.

	s.startModelIteration(runCtx)
}

// foldAssistantAndMaybeDispatch emits the consolidated assistant
// message (FrameObserver routes it into the owned history cache)
// and decides whether the turn ends here (no tool calls) or hands
// off to the tool dispatcher. Sets st.assistantFolded so re-entry
// after the dispatcher exits doesn't re-fold or re-dispatch.
func (s *Session) foldAssistantAndMaybeDispatch(runCtx context.Context) {
	st := s.turnState
	hasToolCalls := len(st.toolCalls) > 0

	// Phase 6.x — TurnFinalizeGate. Consult the gate BEFORE emitting
	// the consolidated assistant message, because a vetoed finalize
	// must emit a NON-terminal (Final=false) message: the subagent
	// executor keys wave-completion off the Final+Consolidated frame
	// (mission OnChildFrame → ingestHandoff → waitForRefs), so emitting
	// Final=true on a blocked iteration would falsely signal the
	// planner is done while the gate is still re-driving it. The gate
	// is skipped past the retry cap (turn retires) and for every
	// session without a declared finalize condition (allow).
	gateBlocks := false
	var continuation string
	if !hasToolCalls && st.finalizeGateRetries < maxFinalizeGateRetries {
		if cont, allow := s.consultTurnFinalizeGate(runCtx, st.finalText); !allow {
			gateBlocks = true
			continuation = cont
		}
	}
	// final marks the turn boundary: no tool calls AND the gate let
	// finalization through. A gate-blocked iteration is Final=false so
	// it folds into history (the planner sees its own prior text) but
	// does not trip the executor's wave-completion detection.
	final := !hasToolCalls && !gateBlocks

	// Persist one consolidated AgentMessage per model iteration: full
	// assembled text + tool calls + reasoning state. Streaming chunks
	// stayed outbox-only — this row is the canonical assistant
	// iteration record that replay reads. Final=true marks the turn
	// boundary (no tool calls, not gate-blocked; turn retires after
	// this); Final=false is a tool-iteration (hands off to the
	// dispatcher) or a gate-blocked iteration (re-iterates the same
	// session). Skipped when the iteration produced nothing.
	if st.agentSeq > 0 || hasToolCalls || st.finalText != "" {
		consolidated := protocol.NewAgentMessageConsolidated(s.id, s.agent.Participant(),
			st.finalText, st.agentSeq, final,
			toolCallPayloads(st.toolCalls),
			st.thinking, st.thoughtSignature)
		// Phase 5.2 (context-budget observability) — stamp turn
		// usage on the Final=true Consolidated frame so the
		// outbox carries the cost number. The cumulative
		// session counter was already folded eagerly in
		// applyChunk on every iter's Final chunk; this stamp is
		// outbox-only.
		if final && (st.turnUsage.PromptTokens > 0 || st.turnUsage.CompletionTokens > 0) {
			snap := st.turnUsage
			consolidated.Payload.Usage = &snap
		}
		_ = s.emit(runCtx, consolidated)
	}
	st.assistantFolded = true

	if gateBlocks {
		// Re-iterate the SAME session: inject the gate's continuation
		// as a system reminder (folded into history), then start a
		// fresh model iteration. resetModelAccumulator (in
		// startModelIteration) clears assistantFolded + the per-iter
		// accumulators. finalizeGateRetries is the per-turn backstop.
		st.finalizeGateRetries++
		s.injectTurnContinuation(runCtx, continuation)
		s.startModelIteration(runCtx)
		return
	}

	if !hasToolCalls {
		s.drainPendingInbound(runCtx)
		s.retireTurn()
		// Lifecycle: turn closed cleanly. Mark idle iff fully
		// quiescent — subagents still running keep the session
		// active (idle requires len(s.children) == 0). retireTurn
		// just nilled turnState; isQuiescent checks the rest.
		if s.isQuiescent() {
			s.markStatus(runCtx, protocol.SessionStatusIdle, "turn_complete")
		}
		// Phase 5.1c.cancel-ux follow-up — if an async mission
		// completed while this turn was in flight, drainPendingInbound
		// above re-emitted its SubagentResult; the compactor folded
		// the inject into the owned history cache. Kick a fresh
		// summary turn so the model presents
		// the result to the user without waiting for another typed
		// message.
		if !s.IsClosing() && s.pendingAsyncSummary.Swap(false) {
			s.kickAsyncSummaryTurn(runCtx)
		}
		return
	}
	// Phase 4.2.3 ε — track main-task tool-call count for the
	// teardown-time SkipIfIdle gate (runCloseTurnSync). All
	// dispatches in this path are main-task; the close turn
	// runs inline in teardown and does not re-enter
	// foldAssistantAndMaybeDispatch.
	s.mainToolCalls.Add(int64(len(st.toolCalls)))

	// η.4 — stuck-detection sampling is now driven by the
	// stuckdetector extension's OnFrameEmit, which observes the
	// consolidated AgentMessage we just emitted above and folds
	// each tool_call into its trailing window. The dispatcher's
	// later tool_result emissions feed the errCode side.

	// Dispatch the tool calls. Each lands a toolResultEvent on
	// s.toolResults; handleToolResult drains pendingToolCalls and
	// the compactor folds the emitted tool_result frames so the
	// next iteration's prompt sees the results.
	for _, tc := range st.toolCalls {
		st.pendingToolCalls[tc.ID] = tc
	}
	ch := make(chan toolResultEvent, 4)
	s.toolResults = ch
	s.turnWG.Add(1)
	go s.runToolDispatcher(s.turnCtx, runCtx, st.toolCalls, ch)
}

// rollbackTurn trims the compactor's owned history cache so any
// consolidated assistant / tool_result entries the cancelled
// turn produced disappear from the model's next view. Called on
// /cancel (no assistant counterpart) or stream error (assistant
// turn never landed).
//
// η.3 semantics: the user message that triggered the turn is
// PRESERVED (its Seq equals the rollback baseline and survives
// the > comparison). The cancel means "abort processing", not
// "rewrite history" — η spec §6 captures the rationale.
func (s *Session) rollbackTurn() {
	st := s.turnState
	if st == nil {
		return
	}
	if owner := s.historyOwner(); owner != nil {
		owner.RollbackTo(context.Background(), s, st.seqBaseline)
	}
}

// retireTurn cancels turnCtx (idempotent), nils per-turn channel
// fields, and clears turnState. After this returns turnComplete()
// reverts to false (no turn) and the loop will sit on s.in / ctx.Done
// until the next inbound frame.
func (s *Session) retireTurn() {
	if s.turnCancel != nil {
		s.turnCancel()
	}
	s.turnCtx = nil
	s.turnCancel = nil
	s.modelChunks = nil
	s.toolResults = nil
	s.turnState = nil
}

// toolCallPayloads projects model.ChunkToolCall slices onto the
// protocol.ToolCallPayload shape used in the consolidated final
// AgentMessage. Drops Hash (used only by the live stuck-detector).
func toolCallPayloads(calls []model.ChunkToolCall) []protocol.ToolCallPayload {
	if len(calls) == 0 {
		return nil
	}
	out := make([]protocol.ToolCallPayload, len(calls))
	for i, c := range calls {
		out[i] = protocol.ToolCallPayload{ToolID: c.ID, Name: c.Name, Args: c.Args}
	}
	return out
}

// drainPendingInbound runs at every turn boundary. Buffered frames
// are persisted (event-source rule §4.3) and — when allow-listed by
// the §11 frame-visibility filter — projected into s.history so the
// next prompt build sees them. Default deny: frames not in the
// allow-list pass through to the outbox + event log but stay out of
// the model's view (e.g. a sub-agent's own tool_call frame if it ever
// reached the parent inbox; not part of the parent's conversation).
//
// emit handles persistence + outbox push together; we layer the
// history projection on top using projectFrameToHistory (§11).
func (s *Session) drainPendingInbound(runCtx context.Context) {
	if len(s.pendingInbound) == 0 {
		return
	}
	for _, f := range s.pendingInbound {
		// η.3 — emit persists + pushes to outbox + notifies the
		// compactor's FrameObserver, which folds allow-listed
		// frames into the owned history cache. emit short-
		// circuits cleanly when the session is mid-shutdown.
		_ = s.emit(runCtx, f)
	}
	s.pendingInbound = s.pendingInbound[:0]
}

// runTurnBoundaryHooks invokes every registered
// [extension.TurnBoundaryHook] in registration order at the
// idle→active boundary. Errors are logged warn-not-fatal — a
// misbehaving hook must not block the new turn.
//
// Hook ordering: matches deps.Extensions slice — extensions
// registered earlier run first. The wiring in
// `pkg/runtime/extensions.go` controls the order; phase 5.2
// places compactor after notepad so any future hook needing
// to read compactor state (e.g. phase 6 scheduler) registers
// later still.
//
// Concurrency: hooks run synchronously on the session's Run
// goroutine. They may block on LLM calls (compactor's β
// summariser) — implementations own their own deadlines /
// ctx-derived timeouts.
func (s *Session) runTurnBoundaryHooks(runCtx context.Context) {
	if s.deps == nil {
		return
	}
	for _, ext := range s.deps.Extensions {
		hook, ok := ext.(extension.TurnBoundaryHook)
		if !ok {
			continue
		}
		if err := hook.OnTurnBoundary(runCtx, s); err != nil {
			s.logger.Warn("session: turn boundary hook failed",
				"session", s.id, "extension", ext.Name(), "err", err)
		}
	}
}
