package session

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/whiteboard"
)

// init registers the four US3 whiteboard tools into the package-level
// dispatch table. Per phase-4-spec §15 step 10 + contracts/tools-
// whiteboard.md these surface as session:whiteboard_init /
// session:whiteboard_write / session:whiteboard_read /
// session:whiteboard_stop.
//
// Permission objects per contracts/permission-objects.md §"Whiteboard
// system tools" — init/write/stop share the write capability; read is
// gated by read.
func init() {
	sessionTools["whiteboard_init"] = sessionToolDescriptor{
		Name:             "whiteboard_init",
		Description:      "Open a broadcast whiteboard on this session. Children spawned afterward can write/read.",
		PermissionObject: permObjectWhiteboardWrite,
		ArgSchema:        json.RawMessage(whiteboardInitSchema),
		Handler:          callWhiteboardInit,
	}
	sessionTools["whiteboard_write"] = sessionToolDescriptor{
		Name:             "whiteboard_write",
		Description:      "Append a broadcast to the whiteboard your parent owns. Every member sees it.",
		PermissionObject: permObjectWhiteboardWrite,
		ArgSchema:        json.RawMessage(whiteboardWriteSchema),
		Handler:          callWhiteboardWrite,
	}
	sessionTools["whiteboard_read"] = sessionToolDescriptor{
		Name:             "whiteboard_read",
		Description:      "Return the retained whiteboard messages — own hosted board if active, else parent's.",
		PermissionObject: permObjectWhiteboardRead,
		ArgSchema:        json.RawMessage(whiteboardReadSchema),
		Handler:          callWhiteboardRead,
	}
	sessionTools["whiteboard_stop"] = sessionToolDescriptor{
		Name:             "whiteboard_stop",
		Description:      "Close the whiteboard hosted on this session. New writes from members surface no_active_whiteboard.",
		PermissionObject: permObjectWhiteboardWrite,
		ArgSchema:        json.RawMessage(whiteboardStopSchema),
		Handler:          callWhiteboardStop,
	}
}

const (
	permObjectWhiteboardWrite = "hugen:whiteboard:write"
	permObjectWhiteboardRead  = "hugen:whiteboard:read"
)

const (
	whiteboardInitSchema = `{
  "type": "object",
  "properties": {}
}`

	whiteboardWriteSchema = `{
  "type": "object",
  "properties": {
    "text": {"type": "string", "description": "Broadcast body. Capped per-message; truncated with marker."}
  },
  "required": ["text"]
}`

	whiteboardReadSchema = `{
  "type": "object",
  "properties": {}
}`

	whiteboardStopSchema = `{
  "type": "object",
  "properties": {}
}`
)

// ---------- whiteboard_init ----------

type whiteboardOKOutput struct {
	OK bool `json:"ok"`
}

func callWhiteboardInit(ctx context.Context, caller *Session, _ SessionToolHost, _ json.RawMessage) (json.RawMessage, error) {
	if caller.IsClosed() {
		return toolErr("session_gone", "calling session has already terminated")
	}
	caller.whiteboardMu.Lock()
	already := caller.whiteboard.Active
	caller.whiteboardMu.Unlock()
	if already {
		// Idempotent — repeat init on an active board returns ok with
		// no change (no event, no projection mutation).
		return json.Marshal(whiteboardOKOutput{OK: true})
	}
	frame := protocol.NewWhiteboardOp(caller.id, "", caller.agent.Participant(),
		protocol.WhiteboardOpPayload{Op: whiteboard.OpInit})
	if err := caller.emit(ctx, frame); err != nil {
		return toolErr("io", fmt.Sprintf("emit whiteboard_op init: %v", err))
	}
	caller.whiteboardMu.Lock()
	caller.whiteboard = whiteboard.Apply(caller.whiteboard, whiteboard.ProjectEvent{
		Op: whiteboard.OpInit,
		At: frame.OccurredAt(),
	})
	caller.whiteboardMu.Unlock()
	return json.Marshal(whiteboardOKOutput{OK: true})
}

// ---------- whiteboard_write ----------

type whiteboardWriteInput struct {
	Text string `json:"text"`
}

func callWhiteboardWrite(ctx context.Context, caller *Session, _ SessionToolHost, args json.RawMessage) (json.RawMessage, error) {
	if caller.IsClosed() {
		return toolErr("session_gone", "calling session has already terminated")
	}
	var in whiteboardWriteInput
	if err := json.Unmarshal(args, &in); err != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid whiteboard_write args: %v", err))
	}
	if in.Text == "" {
		return toolErr("bad_request", "text is required")
	}

	target := caller.parent
	if target == nil {
		// Root sessions write only to their own hosted board if they
		// want to. Per contracts/tools-whiteboard.md, write authors a
		// member-board entry; a root has no parent → no member-board.
		return toolErr("no_whiteboard_to_write_to",
			"root sessions cannot whiteboard_write; only sub-agents broadcast to their parent's board")
	}
	target.whiteboardMu.Lock()
	hostActive := target.whiteboard.Active
	target.whiteboardMu.Unlock()
	if !hostActive {
		return toolErr("no_active_whiteboard",
			"parent session has no active whiteboard to write to")
	}

	role := callerRoleFor(caller)
	frame := protocol.NewWhiteboardOp(target.id, caller.id, caller.agent.Participant(),
		protocol.WhiteboardOpPayload{
			Op:            whiteboard.OpWrite,
			FromSessionID: caller.id,
			FromRole:      role,
			Text:          in.Text,
		})
	if !target.Submit(ctx, frame) {
		return toolErr("io", "host session inbox closed")
	}
	return json.Marshal(whiteboardOKOutput{OK: true})
}

// ---------- whiteboard_read ----------

type whiteboardReadOutput struct {
	Active    bool                       `json:"active"`
	HostID    string                     `json:"host_id,omitempty"`
	StartedAt string                     `json:"started_at,omitempty"`
	NextSeq   int64                      `json:"next_seq,omitempty"`
	Messages  []whiteboardReadMessageRow `json:"messages,omitempty"`
}

type whiteboardReadMessageRow struct {
	Seq           int64  `json:"seq"`
	At            string `json:"at"`
	FromSessionID string `json:"from_session_id,omitempty"`
	FromRole      string `json:"from_role,omitempty"`
	Text          string `json:"text"`
	Truncated     bool   `json:"truncated,omitempty"`
}

func callWhiteboardRead(_ context.Context, caller *Session, _ SessionToolHost, _ json.RawMessage) (json.RawMessage, error) {
	if caller.IsClosed() {
		return toolErr("session_gone", "calling session has already terminated")
	}

	// Per phase-4-spec §7.5: own hosted board takes precedence; if no
	// own active board, fall back to the parent's board the session is
	// a member of.
	source := caller
	hostID := caller.id
	source.whiteboardMu.Lock()
	srcActive := source.whiteboard.Active
	source.whiteboardMu.Unlock()
	if !srcActive && caller.parent != nil {
		source = caller.parent
		hostID = source.id
	}
	source.whiteboardMu.Lock()
	wb := source.whiteboard
	source.whiteboardMu.Unlock()

	if !wb.Active {
		return json.Marshal(whiteboardReadOutput{Active: false})
	}
	out := whiteboardReadOutput{
		Active:    true,
		HostID:    hostID,
		StartedAt: wb.StartedAt.UTC().Format(time.RFC3339),
		NextSeq:   wb.NextSeq,
	}
	if len(wb.Messages) > 0 {
		out.Messages = make([]whiteboardReadMessageRow, 0, len(wb.Messages))
		for _, m := range wb.Messages {
			out.Messages = append(out.Messages, whiteboardReadMessageRow{
				Seq:           m.Seq,
				At:            m.At.UTC().Format(time.RFC3339),
				FromSessionID: m.FromSessionID,
				FromRole:      m.FromRole,
				Text:          m.Text,
				Truncated:     m.Truncated,
			})
		}
	}
	return json.Marshal(out)
}

// ---------- whiteboard_stop ----------

func callWhiteboardStop(ctx context.Context, caller *Session, _ SessionToolHost, _ json.RawMessage) (json.RawMessage, error) {
	if caller.IsClosed() {
		return toolErr("session_gone", "calling session has already terminated")
	}
	caller.whiteboardMu.Lock()
	active := caller.whiteboard.Active
	caller.whiteboardMu.Unlock()
	if !active {
		// Idempotent — stop on a closed board returns ok.
		return json.Marshal(whiteboardOKOutput{OK: true})
	}
	frame := protocol.NewWhiteboardOp(caller.id, "", caller.agent.Participant(),
		protocol.WhiteboardOpPayload{Op: whiteboard.OpStop})
	if err := caller.emit(ctx, frame); err != nil {
		return toolErr("io", fmt.Sprintf("emit whiteboard_op stop: %v", err))
	}
	caller.whiteboardMu.Lock()
	caller.whiteboard = whiteboard.Apply(caller.whiteboard, whiteboard.ProjectEvent{
		Op: whiteboard.OpStop,
		At: frame.OccurredAt(),
	})
	caller.whiteboardMu.Unlock()
	return json.Marshal(whiteboardOKOutput{OK: true})
}

// callerRoleFor extracts the sub-agent's role for whiteboard
// attribution. Falls back to the agent ID when no sub-agent role is
// associated (e.g. a root session writing to its own — though the
// write tool currently rejects that case before we get here).
func callerRoleFor(s *Session) string {
	if s == nil {
		return ""
	}
	if s.agent != nil {
		return s.agent.ID()
	}
	return ""
}
