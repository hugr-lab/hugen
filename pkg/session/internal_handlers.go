package session

import (
	"context"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/whiteboard"
)

// internalHandler runs a synchronous side-effect for a RouteInternal
// Frame. The handler may emit further Frames (via s.emit) or trigger
// downstream side-effects (e.g. m.Deliver to siblings); the Frame
// itself never lands in s.history or s.pendingInbound. Errors are
// logged inside the handler — the dispatcher's contract is "best
// effort, fire-and-forget".
//
// Phase 4 step 10 (whiteboard primitive) registers two entries:
//   - KindWhiteboardOp on the host side: a child wrote to its
//     parent's board; persist the host's own whiteboard_op{op:"write",
//     seq} and broadcast a whiteboard_message Frame to each member.
//   - KindWhiteboardMessage on the member side: the host broadcast a
//     write; persist the member's local whiteboard_op{op:"write"},
//     update the in-memory projection, append a formatted system_message
//     into history so the model sees it on its next prompt build.
//
// Phase 5 may register HITL forwarding handlers here as well.
type internalHandler func(s *Session, ctx context.Context, f protocol.Frame)

// internalHandlers maps each RouteInternal Frame Kind to its sync
// side-effect handler. Kept package-level + immutable after init so
// the hot path doesn't need a lock: register at init time, read on
// every routed frame.
var internalHandlers = map[protocol.Kind]internalHandler{
	protocol.KindWhiteboardOp:      handleWhiteboardOpInbound,
	protocol.KindWhiteboardMessage: handleWhiteboardMessageInbound,
}

// dispatchInternal looks up and invokes the registered handler for
// the Frame's Kind. No-op (with a debug log) when no handler is
// registered — spec §10.2 says RouteInternal Frames "trigger a
// runtime side-effect that does not need to reach the model", so a
// missing handler means the routing table is wrong, not that the
// Frame should fall through to history.
func (s *Session) dispatchInternal(ctx context.Context, f protocol.Frame) {
	if h, ok := internalHandlers[f.Kind()]; ok {
		h(s, ctx, f)
		return
	}
	s.logger.Debug("session: RouteInternal frame with no handler registered",
		"session", s.id, "kind", f.Kind())
}

// handleWhiteboardOpInbound runs on the **host** session when a child
// delivers a WhiteboardOp Frame (op="write") via parent.Submit. Per
// phase-4-spec §7.4:
//
//  1. If the host has no active board, drop the write — the child's
//     view of the board is stale (host stopped between dispatch and
//     receipt). Logged at Debug; the child sees its tool succeed but
//     no broadcast comes back. The next time the child calls
//     whiteboard_write the parent.whiteboard.Active check at the
//     tool layer will fail cleanly.
//
//  2. Allocate a host-monotonic seq, persist a whiteboard_op{op:"write",
//     seq, from_session_id, from_role, text, truncated} event in the
//     host's events, and apply to the host's projection.
//
//  3. Build a whiteboard_message Frame and Submit it to every direct
//     child (including the author so the author sees its own write
//     surface back as part of the consistent broadcast stream).
//
// Inbound op="init" / op="stop" Frames are not expected — the tools
// run on the host's own goroutine and emit those events directly.
// Logged at Warn in case a future phase wires cross-session init.
func handleWhiteboardOpInbound(s *Session, ctx context.Context, f protocol.Frame) {
	op, ok := f.(*protocol.WhiteboardOp)
	if !ok {
		return
	}
	if op.Payload.Op != whiteboard.OpWrite {
		s.logger.Warn("session: unexpected whiteboard_op kind on host inbound",
			"session", s.id, "op", op.Payload.Op)
		return
	}

	from := op.FromSessionID()
	if from == "" {
		from = op.Payload.FromSessionID
	}
	fromRole := op.Payload.FromRole

	// Truncate over-long text and stamp Truncated true so the broadcast
	// + persisted events both record the cut.
	text, truncated := truncateWhiteboardText(op.Payload.Text)
	if op.Payload.Truncated {
		truncated = true
	}

	s.whiteboardMu.Lock()
	if !s.whiteboard.Active {
		s.whiteboardMu.Unlock()
		s.logger.Debug("session: whiteboard write to inactive host board dropped",
			"session", s.id, "from", from)
		return
	}
	seq := s.whiteboard.NextSeq
	if seq == 0 {
		seq = 1
	}
	hostEvent := whiteboard.ProjectEvent{
		Op:            whiteboard.OpWrite,
		Seq:           seq,
		At:            f.OccurredAt(),
		FromSessionID: from,
		FromRole:      fromRole,
		Text:          text,
		Truncated:     truncated,
	}
	s.whiteboard = whiteboard.Apply(s.whiteboard, hostEvent)
	s.whiteboardMu.Unlock()

	// Persist host's own whiteboard_op{op:"write", seq} event.
	hostFrame := protocol.NewWhiteboardOp(s.id, from, s.agent.Participant(), protocol.WhiteboardOpPayload{
		Op:            whiteboard.OpWrite,
		Seq:           seq,
		FromSessionID: from,
		FromRole:      fromRole,
		Text:          text,
		Truncated:     truncated,
	})
	if err := s.emit(ctx, hostFrame); err != nil {
		s.logger.Warn("session: persist host whiteboard_op",
			"session", s.id, "err", err)
		// Continue with the broadcast — the projection is already
		// updated; the missing event will reconcile on next materialise
		// since events are the source of truth.
	}

	// Broadcast to every direct child (active members of the board).
	// Skip closed children; collect under the lock then send outside
	// to avoid holding childMu across Submit.
	s.childMu.Lock()
	members := make([]*Session, 0, len(s.children))
	for _, c := range s.children {
		if c == nil || c.IsClosed() {
			continue
		}
		members = append(members, c)
	}
	s.childMu.Unlock()
	for _, m := range members {
		bm := protocol.NewWhiteboardMessage(s.id, m.id, s.agent.Participant(),
			protocol.WhiteboardMessagePayload{
				FromSessionID: from,
				FromRole:      fromRole,
				Seq:           seq,
				Text:          text,
			})
		if !m.Submit(ctx, bm) {
			s.logger.Debug("session: whiteboard broadcast Submit failed",
				"host", s.id, "member", m.id)
		}
	}
}

// handleWhiteboardMessageInbound runs on a **member** session when its
// host delivers a whiteboard_message Frame. Per phase-4-spec §7.4
// step 3:
//
//  1. Persist a local whiteboard_op{op:"write", seq, from_session_id,
//     from_role, text} event in this session's events. The Seq carried
//     in the broadcast (host-monotonic) flows through unchanged.
//  2. Update the member's in-memory projection.
//  3. Append a formatted system_message{kind:"whiteboard"} so audit
//     and adapters see it; mirror the same line into s.history so the
//     model sees the broadcast on its next prompt build.
//
// The original whiteboard_message Frame is discarded — RouteInternal
// guarantees it never reaches s.history or s.pendingInbound.
func handleWhiteboardMessageInbound(s *Session, ctx context.Context, f protocol.Frame) {
	bm, ok := f.(*protocol.WhiteboardMessage)
	if !ok {
		return
	}
	p := bm.Payload

	s.whiteboardMu.Lock()
	if !s.whiteboard.Active {
		// Synthesise an init event so the member-side projection has a
		// boundary to attach OpWrite events to. Reflects "I joined the
		// board on first broadcast" — no separate init Frame is sent.
		s.whiteboard = whiteboard.Apply(s.whiteboard, whiteboard.ProjectEvent{
			Op: whiteboard.OpInit,
			At: f.OccurredAt(),
		})
	}
	s.whiteboard = whiteboard.Apply(s.whiteboard, whiteboard.ProjectEvent{
		Op:            whiteboard.OpWrite,
		Seq:           p.Seq,
		At:            f.OccurredAt(),
		FromSessionID: p.FromSessionID,
		FromRole:      p.FromRole,
		Text:          p.Text,
	})
	s.whiteboardMu.Unlock()

	// Persist the member's own whiteboard_op event.
	memberOp := protocol.NewWhiteboardOp(s.id, "", s.agent.Participant(), protocol.WhiteboardOpPayload{
		Op:            whiteboard.OpWrite,
		Seq:           p.Seq,
		FromSessionID: p.FromSessionID,
		FromRole:      p.FromRole,
		Text:          p.Text,
	})
	if err := s.emit(ctx, memberOp); err != nil {
		s.logger.Warn("session: persist member whiteboard_op",
			"session", s.id, "err", err)
	}

	// Surface to model + adapters as a system_message{kind:"whiteboard"}.
	formatted := formatWhiteboardLine(p.FromRole, p.FromSessionID, p.Text)
	sm := protocol.NewSystemMessage(s.id, s.agent.Participant(),
		protocol.SystemMessageWhiteboard, formatted)
	if err := s.emit(ctx, sm); err != nil {
		s.logger.Warn("session: emit whiteboard system_message",
			"session", s.id, "err", err)
	}

	// Mirror into in-memory history so the model sees the broadcast at
	// its next prompt build. Provider-portable encoding: Role=user with
	// a "[system: whiteboard]" prefix.
	s.history = append(s.history, model.Message{
		Role:    model.RoleUser,
		Content: "[system: " + protocol.SystemMessageWhiteboard + "] " + formatted,
	})
}

// truncateWhiteboardText is a thin wrapper around the projection
// package's per-message cap so callers (tools + handlers) can clip
// before persistence and broadcast. Mirrors whiteboard.truncate; the
// duplication keeps the projection package free of any outward
// dependency.
func truncateWhiteboardText(text string) (string, bool) {
	if len(text) <= whiteboard.MaxMessageBytes {
		return text, false
	}
	cut := whiteboard.MaxMessageBytes - len(whiteboard.TruncationMarker)
	if cut < 0 {
		cut = 0
	}
	return text[:cut] + whiteboard.TruncationMarker, true
}

// formatWhiteboardLine renders a broadcast as the canonical text line
// the spec describes: `<role> (<session>): <text>`. role / session
// fall back to placeholder strings when missing so a malformed
// payload still produces a readable line.
func formatWhiteboardLine(role, sessionID, text string) string {
	if role == "" {
		role = "subagent"
	}
	if sessionID == "" {
		sessionID = "?"
	}
	return fmt.Sprintf("%s (%s): %s", role, sessionID, text)
}
