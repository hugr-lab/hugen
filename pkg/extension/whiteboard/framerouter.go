package whiteboard

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// HandleFrame implements [extension.FrameRouter]. The session's
// inbound dispatcher routes every [protocol.KindExtensionFrame]
// addressed to the "whiteboard" extension here. Two cross-session
// flows land on this method:
//
//   - "write" — a sub-agent's whiteboard:write tool delivered an
//     ExtensionFrame{op:"write"} to its parent (host). The handler
//     allocates the host-monotonic seq, persists the canonical
//     write event on the host, applies it to the projection, and
//     broadcasts an op:"message" frame to every direct child
//     (including the author so the broadcast stream is consistent).
//
//   - "message" — a host fanned a write out to this member. The
//     handler synthesises an init boundary on the member if absent,
//     applies the write to the member's projection, persists the
//     local write event, and Submits a [protocol.SystemMessage]
//     {kind:"whiteboard"} back into this session's inbox so the
//     model sees the broadcast on the next prompt build (carried
//     through the standard RouteBuffered path; no direct history
//     mutation).
//
// op:"init" / op:"stop" frames arrive only via state.Emit on the
// owning host session — they never enter s.in cross-session, so a
// surprise inbound is logged and dropped.
func (e *Extension) HandleFrame(ctx context.Context, state extension.SessionState, f *protocol.ExtensionFrame) error {
	h := FromState(state)
	if h == nil {
		return nil
	}
	switch f.Payload.Op {
	case OpWrite:
		return e.handleHostInboundWrite(ctx, state, h, f)
	case OpMessage:
		return e.handleMemberBroadcast(ctx, state, h, f)
	case OpInit, OpStop:
		// In-session emits never route through HandleFrame; getting
		// one here means a cross-session sender mis-addressed the
		// frame. Silently ignore.
		return nil
	default:
		return nil
	}
}

func (e *Extension) handleHostInboundWrite(ctx context.Context, state extension.SessionState, h *SessionWhiteboard, f *protocol.ExtensionFrame) error {
	var in writeData
	if len(f.Payload.Data) > 0 {
		if err := json.Unmarshal(f.Payload.Data, &in); err != nil {
			return fmt.Errorf("decode whiteboard:write payload: %w", err)
		}
	}
	from := in.FromSessionID
	if from == "" {
		from = f.FromSessionID()
	}
	role := in.FromRole

	text, truncated := truncate(in.Text)
	if in.Truncated {
		truncated = true
	}

	h.mu.Lock()
	if !h.wb.Active {
		h.mu.Unlock()
		// Drop silently — the child's view of the board is stale
		// (host stopped between dispatch and receipt). The next
		// child whiteboard:write will surface no_active_whiteboard
		// at the tool layer.
		return nil
	}
	seq := h.wb.NextSeq
	if seq == 0 {
		seq = 1
	}
	at := f.OccurredAt()
	h.wb = Apply(h.wb, ProjectEvent{
		Op:            OpWrite,
		Seq:           seq,
		At:            at,
		FromSessionID: from,
		FromRole:      role,
		Text:          text,
		Truncated:     truncated,
	})
	h.mu.Unlock()

	canonical := writeData{
		Seq:           seq,
		FromSessionID: from,
		FromRole:      role,
		Text:          text,
		Truncated:     truncated,
	}

	// Persist host's own canonical write event with the assigned seq.
	hostFrame, err := newOpFrame(state.SessionID(), from, e.agentParticipant(), OpWrite, canonical)
	if err != nil {
		return err
	}
	if err := state.Emit(ctx, hostFrame); err != nil {
		// The projection is already updated; the missing event
		// reconciles on next materialise since events are the
		// source of truth.
		return fmt.Errorf("persist host whiteboard:write: %w", err)
	}

	// Broadcast to every direct child (active members of the board).
	// Skip closed children — Submit returns false on a closed inbox
	// and we just drop those.
	for _, child := range state.Children() {
		bm, err := newOpFrame(child.SessionID(), state.SessionID(), e.agentParticipant(), OpMessage, canonical)
		if err != nil {
			continue
		}
		_ = child.Submit(ctx, bm)
	}
	return nil
}

func (e *Extension) handleMemberBroadcast(ctx context.Context, state extension.SessionState, h *SessionWhiteboard, f *protocol.ExtensionFrame) error {
	var in writeData
	if len(f.Payload.Data) > 0 {
		if err := json.Unmarshal(f.Payload.Data, &in); err != nil {
			return fmt.Errorf("decode whiteboard:message payload: %w", err)
		}
	}
	at := f.OccurredAt()

	h.mu.Lock()
	needInit := !h.wb.Active
	if needInit {
		// Synthesise an init boundary so the member-side projection
		// has somewhere to attach OpWrite — reflects "I joined the
		// board on first broadcast". The synthetic init is persisted
		// below so a restart's Recover sees it (without a persisted
		// init the writes drop in Apply's defensive guard).
		h.wb = Apply(h.wb, ProjectEvent{Op: OpInit, At: at})
	}
	h.wb = Apply(h.wb, ProjectEvent{
		Op:            OpWrite,
		Seq:           in.Seq,
		At:            at,
		FromSessionID: in.FromSessionID,
		FromRole:      in.FromRole,
		Text:          in.Text,
		Truncated:     in.Truncated,
	})
	h.mu.Unlock()

	// Persist the synthetic init first when the member is observing
	// a board for the first time. Recovery applies events in order
	// so the init must precede the write in the log.
	if needInit {
		initFrame, err := newOpFrame(state.SessionID(), "", e.agentParticipant(), OpInit, nil)
		if err != nil {
			return err
		}
		if err := state.Emit(ctx, initFrame); err != nil {
			return fmt.Errorf("persist member whiteboard:init: %w", err)
		}
	}

	// Persist the member's own write event so a restart reconstructs
	// the projection from this session's events alone.
	memberFrame, err := newOpFrame(state.SessionID(), in.FromSessionID, e.agentParticipant(), OpWrite, in)
	if err != nil {
		return err
	}
	if err := state.Emit(ctx, memberFrame); err != nil {
		return fmt.Errorf("persist member whiteboard:write: %w", err)
	}

	// Surface the broadcast to the model via a system_message frame
	// routed back through this session's inbox. RouteBuffered drains
	// it at the next turn boundary, persists it, and folds it into
	// s.history through the visibility filter — no direct history
	// mutation from extension code.
	line := formatLine(in.FromRole, in.FromSessionID, in.Text)
	sm := protocol.NewSystemMessage(state.SessionID(), e.agentParticipant(),
		protocol.SystemMessageWhiteboard, line)
	_ = state.Submit(ctx, sm)
	return nil
}

// formatLine renders a broadcast as the canonical text line the
// spec describes: `<role> (<session>): <text>`. Falls back to
// placeholder strings on missing fields so a malformed payload
// still produces a readable line.
func formatLine(role, sessionID, text string) string {
	if role == "" {
		role = "subagent"
	}
	if sessionID == "" {
		sessionID = "?"
	}
	return fmt.Sprintf("%s (%s): %s", role, sessionID, text)
}
