package session

import (
	"context"
	"encoding/json"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// runCloseTurnSync drives the deterministic close turn entirely
// inside teardown's goroutine — no per-turn goroutines, no
// channels, no Run-loop re-entry. teardown calls this right
// after observing a SessionClose, before the existing teardown
// sequence cancels the in-flight turn / cascades to children /
// dispatches Closers / writes the SessionTerminated row.
//
// Lifecycle invariant the rewrite preserves: subagents
// terminate **only** in response to an inbox SessionClose;
// they never self-close via flags. The previous deferred-
// startTurn implementation violated this — a model failure
// inside the synthetic turn left the session idle with a stale
// `closeTurn != nil`, and the parent.children map kept the
// zombie indefinitely. This sync runner closes that hole: any
// failure during the close turn returns an error to the
// caller (teardown), teardown logs and proceeds with the
// existing cleanup steps, so child.Run always exits and its
// defer chain (close(s.out) + close(s.done)) lets the parent's
// deregister callback prune the child handle.
//
// The runner mirrors the regular turn machinery's contract
// (Stream.Next loop, per-iteration consolidated AgentMessage,
// tool dispatch through dispatchToolCall) but skips:
//
//   - Streaming chunk emits — the outbox is about to close;
//     adapters won't render mid-teardown chunks. Only the
//     consolidated AgentMessage per iteration is persisted.
//   - turnState / turnWG / turnCancel — the close turn is
//     scoped to this function call; no shared state lives past
//     return.
//   - Stuck detection / soft-warning nudges — the close turn
//     is bounded by block.MaxTurns; the regular soft/hard cap
//     machinery doesn't apply.
//
// Returns an error iff the iteration could not start or the
// model returned an unrecoverable error. Tool dispatch errors
// inside an iteration are surfaced as tool_error frames (per
// dispatchToolCall) and recorded into history so the model can
// react on the next iteration; they don't propagate out.
func (s *Session) runCloseTurnSync(ctx context.Context, block extension.CloseTurnBlock) error {
	if block.IsEmpty() {
		return nil
	}
	spec := buildCloseTurnState(block, s.closeReason)

	mdl, _, err := s.models.Resolve(ctx, model.Hint{
		Intent:        s.DefaultIntent(),
		SessionModels: s.sessionModels(),
	})
	if err != nil {
		return err
	}

	if err := s.materialise(ctx); err != nil {
		s.logger.Warn("close turn: materialise history",
			"session", s.id, "err", err)
	}

	// Persist the synthetic user message so replay sees the
	// close turn as a normal conversational turn. Author is the
	// agent participant — there is no human caller; the runtime
	// speaks on behalf of the system.
	promptText := closeTurnPromptOrDefault(block)
	synth := protocol.NewUserMessage(s.id, s.agent.Participant(), promptText)
	if err := s.persistOnly(ctx, synth); err != nil {
		s.logger.Warn("close turn: persist synthetic user message",
			"session", s.id, "err", err)
	}

	tools, err := s.buildCloseTurnTools(ctx, spec.AllowedTools)
	if err != nil {
		s.logger.Warn("close turn: build tool catalogue",
			"session", s.id, "err", err)
	}

	msgs := append([]model.Message(nil), s.history...)
	msgs = append(msgs, model.Message{
		Role:    model.RoleUser,
		Content: promptText,
	})

	for iter := 0; iter < spec.MaxTurns; iter++ {
		req := model.Request{Messages: msgs, Tools: tools}
		stream, err := mdl.Generate(ctx, req)
		if err != nil {
			s.logger.Warn("close turn: model.Generate",
				"session", s.id, "iter", iter, "err", err)
			return err
		}
		text, reasoning, signature, toolCalls, drainErr := drainCloseTurnStream(ctx, stream)
		_ = stream.Close()
		if drainErr != nil {
			s.logger.Warn("close turn: drain stream",
				"session", s.id, "iter", iter, "err", drainErr)
			return drainErr
		}

		final := len(toolCalls) == 0
		if text != "" || len(toolCalls) > 0 || reasoning != "" {
			consolidated := protocol.NewAgentMessageConsolidated(
				s.id, s.agent.Participant(),
				text, 0, final,
				toolCallPayloads(toolCalls),
				reasoning, signature)
			if err := s.persistOnly(ctx, consolidated); err != nil {
				s.logger.Warn("close turn: persist consolidated agent_message",
					"session", s.id, "iter", iter, "err", err)
			}
		}

		msgs = append(msgs, model.Message{
			Role:             model.RoleAssistant,
			Content:          text,
			ToolCalls:        toolCalls,
			Thinking:         reasoning,
			ThoughtSignature: signature,
		})

		if final {
			return nil
		}

		for _, tc := range toolCalls {
			result, _ := s.dispatchToolCall(ctx, ctx, tc)
			msgs = append(msgs, model.Message{
				Role:       model.RoleTool,
				Content:    result,
				ToolCallID: tc.ID,
			})
		}
	}

	s.logger.Info("close turn: max_turns exhausted",
		"session", s.id, "max_turns", spec.MaxTurns)
	return nil
}

// drainCloseTurnStream consumes Stream.Next synchronously until
// the stream signals exhaustion or returns an error. Mirrors
// applyChunk's accumulator but does NOT emit per-chunk frames —
// the close turn only persists the per-iteration consolidated
// AgentMessage (mid-teardown the outbox is about to close;
// streaming chunks add log noise without benefit).
func drainCloseTurnStream(ctx context.Context, stream model.Stream) (text, reasoning, signature string, toolCalls []model.ChunkToolCall, err error) {
	for {
		chunk, more, nerr := stream.Next(ctx)
		if nerr != nil {
			return text, reasoning, signature, toolCalls, nerr
		}
		if chunk.Content != nil {
			text += *chunk.Content
		}
		if chunk.Reasoning != nil {
			reasoning += *chunk.Reasoning
		}
		if chunk.Thinking != "" {
			reasoning += chunk.Thinking
		}
		if chunk.ThoughtSignature != "" {
			signature = chunk.ThoughtSignature
		}
		if chunk.ToolCall != nil {
			toolCalls = append(toolCalls, *chunk.ToolCall)
		}
		if !more {
			return text, reasoning, signature, toolCalls, nil
		}
	}
}

// buildCloseTurnTools fetches the session's tool snapshot and
// narrows it to the names in allowed. Conversion mirrors
// modelToolsForSession's inline path.
func (s *Session) buildCloseTurnTools(ctx context.Context, allowed []string) ([]model.Tool, error) {
	if s.tools == nil || len(allowed) == 0 {
		return nil, nil
	}
	snap, err := s.fetchSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	allow := make(map[string]struct{}, len(allowed))
	for _, n := range allowed {
		allow[n] = struct{}{}
	}
	out := make([]model.Tool, 0, len(allowed))
	for _, t := range snap.Tools {
		if _, ok := allow[t.Name]; !ok {
			continue
		}
		var schema map[string]any
		if len(t.ArgSchema) > 0 {
			if err := json.Unmarshal(t.ArgSchema, &schema); err != nil {
				s.logger.Warn("close turn: bad tool arg schema",
					"session", s.id, "tool", t.Name, "err", err)
				schema = nil
			}
		}
		out = append(out, model.Tool{
			Name:        t.Name,
			Description: t.Description,
			Schema:      schema,
		})
	}
	return out, nil
}
