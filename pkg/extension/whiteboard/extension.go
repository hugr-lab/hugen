package whiteboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// StateKey is the [extension.SessionState] key the extension stores
// its per-session [*SessionWhiteboard] handle under. Exported so
// callers outside the extension can recover the handle without
// magic strings.
const StateKey = "whiteboard"

// providerName is the catalogue prefix the LLM sees:
// "whiteboard:<tool>". Doubles as [Extension.Name] and the
// [protocol.ExtensionFrame.Payload.Extension] discriminator.
const providerName = "whiteboard"

// Permission objects the runtime gates the whiteboard tools on.
// Mirrored from the legacy session: tool entries so existing config
// keeps working.
const (
	PermObjectWrite = "hugen:whiteboard:write"
	PermObjectRead  = "hugen:whiteboard:read"
)

// Extension wires the whiteboard tool surface, projection, and
// host/member broadcast handlers into the session capability
// pipeline. The instance is shared across every session under one
// Manager; per-session state lives in [extension.SessionState] under
// [StateKey].
type Extension struct {
	agentID string
}

// NewExtension constructs the whiteboard extension. agentID stamps
// the ParticipantInfo on every emitted frame.
func NewExtension(agentID string) *Extension {
	return &Extension{agentID: agentID}
}

var (
	_ extension.Extension              = (*Extension)(nil)
	_ extension.StateInitializer       = (*Extension)(nil)
	_ extension.Recovery               = (*Extension)(nil)
	_ extension.FrameRouter            = (*Extension)(nil)
	_ extension.WhiteboardSystemWriter = (*Extension)(nil)
	_ tool.ToolProvider                = (*Extension)(nil)
)

// Name implements [extension.Extension] and [tool.ToolProvider].
func (e *Extension) Name() string { return providerName }

// Lifetime implements [tool.ToolProvider]. Whiteboard state lives
// in [extension.SessionState]; the provider itself is shared so
// PerAgent fits.
func (e *Extension) Lifetime() tool.Lifetime { return tool.LifetimePerAgent }

// agentParticipant returns the ParticipantInfo whiteboard ext stamps
// on every emitted frame. The whiteboard tool path is always invoked
// by the agent (the model issues the tool call, or the host runtime
// fans out a broadcast), so the author is the agent.
func (e *Extension) agentParticipant() protocol.ParticipantInfo {
	return protocol.ParticipantInfo{ID: e.agentID, Kind: protocol.ParticipantAgent}
}

// InitState implements [extension.StateInitializer]. Allocates a
// fresh [SessionWhiteboard] handle for the calling session and
// stashes it under [StateKey]. The handle starts inactive; init
// from the tool path or [Recover] flips it on.
func (e *Extension) InitState(_ context.Context, state extension.SessionState) error {
	state.SetValue(StateKey, &SessionWhiteboard{
		sessionID: state.SessionID(),
		author:    e.agentParticipant(),
	})
	return nil
}

// FromState returns the [*SessionWhiteboard] handle for state, or
// nil if the extension has not run InitState for it.
func FromState(state extension.SessionState) *SessionWhiteboard {
	if state == nil {
		return nil
	}
	v, ok := state.Value(StateKey)
	if !ok {
		return nil
	}
	h, _ := v.(*SessionWhiteboard)
	return h
}

// SessionWhiteboard is the per-session typed handle the whiteboard
// extension stores in [extension.SessionState]. Owns the in-memory
// [Whiteboard] projection plus a mutex serialising the host fan-in
// + tool emit-then-Apply paths so concurrent writers can't race.
// Persisted events stay the source of truth; a desync self-heals on
// the next materialise / restart.
type SessionWhiteboard struct {
	sessionID string
	author    protocol.ParticipantInfo

	mu sync.Mutex
	wb Whiteboard
}

// Snapshot returns a deep copy of the in-memory projection. Tests
// use this to assert whiteboard state without poking handle
// internals.
func (h *SessionWhiteboard) Snapshot() Whiteboard {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.wb
	if len(out.Messages) > 0 {
		out.Messages = append([]Message(nil), out.Messages...)
	}
	return out
}

// WhiteboardSnapshot implements [extension.WhiteboardContributor].
// Returns a one-line-per-message rendering of the session's
// whiteboard (or the parent's, when this session has no own
// active board — same precedence as `read`). Empty string when
// no board is active anywhere up the chain. Phase 5.1b §3.
func (e *Extension) WhiteboardSnapshot(_ context.Context, state extension.SessionState) string {
	h := FromState(state)
	if h == nil {
		return ""
	}
	source := h
	source.mu.Lock()
	active := source.wb.Active
	source.mu.Unlock()
	if !active {
		if parentState, ok := state.Parent(); ok {
			if parentH := FromState(parentState); parentH != nil {
				source = parentH
			}
		}
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if !source.wb.Active || len(source.wb.Messages) == 0 {
		return ""
	}
	var b strings.Builder
	for i, m := range source.wb.Messages {
		if i > 0 {
			b.WriteByte('\n')
		}
		if m.FromRole != "" {
			b.WriteString("[" + m.FromRole + "] ")
		}
		b.WriteString(m.Text)
	}
	return b.String()
}

// ---------- ToolProvider surface ----------

const (
	initSchema = `{
  "type": "object",
  "properties": {}
}`

	writeSchema = `{
  "type": "object",
  "properties": {
    "text": {"type": "string", "description": "Broadcast body. Capped per-message; truncated with marker."}
  },
  "required": ["text"]
}`

	readSchema = `{
  "type": "object",
  "properties": {}
}`

	stopSchema = `{
  "type": "object",
  "properties": {}
}`
)

// List implements [tool.ToolProvider].
func (e *Extension) List(_ context.Context) ([]tool.Tool, error) {
	return []tool.Tool{
		{
			Name:             providerName + ":init",
			Description:      "Open a broadcast whiteboard on this session. Children spawned afterward can write/read.",
			Provider:         providerName,
			PermissionObject: PermObjectWrite,
			ArgSchema:        json.RawMessage(initSchema),
		},
		{
			Name:             providerName + ":write",
			Description:      "Append a broadcast to the whiteboard your parent owns. Every member sees it.",
			Provider:         providerName,
			PermissionObject: PermObjectWrite,
			ArgSchema:        json.RawMessage(writeSchema),
		},
		{
			Name:             providerName + ":read",
			Description:      "Return the retained whiteboard messages — own hosted board if active, else parent's.",
			Provider:         providerName,
			PermissionObject: PermObjectRead,
			ArgSchema:        json.RawMessage(readSchema),
		},
		{
			Name:             providerName + ":stop",
			Description:      "Close the whiteboard hosted on this session. New writes from members surface no_active_whiteboard.",
			Provider:         providerName,
			PermissionObject: PermObjectWrite,
			ArgSchema:        json.RawMessage(stopSchema),
		},
	}, nil
}

// Call implements [tool.ToolProvider]. Routes by short tool name
// after stripping the "whiteboard:" prefix.
func (e *Extension) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	short := strings.TrimPrefix(name, providerName+":")
	state, ok := extension.SessionStateFromContext(ctx)
	if !ok || state == nil {
		return toolErr("session_gone", "no session attached to dispatch ctx")
	}
	h := FromState(state)
	if h == nil {
		return toolErr("unavailable", "whiteboard extension state not initialised")
	}
	switch short {
	case "init":
		return e.callInit(ctx, state, h)
	case "write":
		return e.callWrite(ctx, state, h, args)
	case "read":
		return e.callRead(state, h)
	case "stop":
		return e.callStop(ctx, state, h)
	default:
		return nil, fmt.Errorf("%w: whiteboard:%s", tool.ErrUnknownTool, short)
	}
}

// Subscribe implements [tool.ToolProvider]. Catalogue is static.
func (e *Extension) Subscribe(_ context.Context) (<-chan tool.ProviderEvent, error) {
	return nil, nil
}

// Close implements [tool.ToolProvider]. Per-session state lives in
// the SessionState bag and is GC'd with the handle; nothing for the
// provider to release.
func (e *Extension) Close() error { return nil }

// ---------- handlers ----------

type writeInput struct {
	Text string `json:"text"`
}

type okOutput struct {
	OK bool `json:"ok"`
}

type readOutput struct {
	Active    bool             `json:"active"`
	HostID    string           `json:"host_id,omitempty"`
	StartedAt string           `json:"started_at,omitempty"`
	NextSeq   int64            `json:"next_seq,omitempty"`
	Messages  []readMessageRow `json:"messages,omitempty"`
}

type readMessageRow struct {
	Seq           int64  `json:"seq"`
	At            string `json:"at"`
	FromSessionID string `json:"from_session_id,omitempty"`
	FromRole      string `json:"from_role,omitempty"`
	Text          string `json:"text"`
	Truncated     bool   `json:"truncated,omitempty"`
}

type toolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type toolErrorResponse struct {
	Error toolError `json:"error"`
}

func toolErr(code, msg string) (json.RawMessage, error) {
	return json.Marshal(toolErrorResponse{Error: toolError{Code: code, Message: msg}})
}

// SystemInit opens a whiteboard on the calling session's state
// via the system principal — no ToolManager dispatch, no LLM
// round-trip, no permission gate. Used by the runtime to seed a
// mission's whiteboard from its skill's metadata.hugen.mission.on_start.whiteboard
// block before the mission's first model turn (phase 4.2.2 §7).
// Idempotent — re-init on an active board is a no-op.
//
// Authorised callers: pkg/session/spawn.go only.
func (e *Extension) SystemInit(ctx context.Context, state extension.SessionState) error {
	if state == nil {
		return errors.New("whiteboard: SystemInit: state is nil")
	}
	h := FromState(state)
	if h == nil {
		return errors.New("whiteboard: SystemInit: no whiteboard handle on session state")
	}
	h.mu.Lock()
	already := h.wb.Active
	h.mu.Unlock()
	if already {
		return nil
	}
	frame, err := newOpFrame(state.SessionID(), "", e.agentParticipant(), OpInit, nil)
	if err != nil {
		return fmt.Errorf("whiteboard: SystemInit: build frame: %w", err)
	}
	if err := state.Emit(ctx, frame); err != nil {
		return fmt.Errorf("whiteboard: SystemInit: emit: %w", err)
	}
	h.mu.Lock()
	h.wb = Apply(h.wb, ProjectEvent{Op: OpInit, At: frame.OccurredAt()})
	h.mu.Unlock()
	return nil
}

func (e *Extension) callInit(ctx context.Context, state extension.SessionState, h *SessionWhiteboard) (json.RawMessage, error) {
	h.mu.Lock()
	already := h.wb.Active
	h.mu.Unlock()
	if already {
		// Idempotent — repeat init on an active board returns ok with
		// no change (no event, no projection mutation).
		return json.Marshal(okOutput{OK: true})
	}
	frame, err := newOpFrame(state.SessionID(), "", e.agentParticipant(), OpInit, nil)
	if err != nil {
		return toolErr("io", err.Error())
	}
	if err := state.Emit(ctx, frame); err != nil {
		return toolErr("io", fmt.Sprintf("emit whiteboard:init: %v", err))
	}
	h.mu.Lock()
	h.wb = Apply(h.wb, ProjectEvent{Op: OpInit, At: frame.OccurredAt()})
	h.mu.Unlock()
	return json.Marshal(okOutput{OK: true})
}

func (e *Extension) callWrite(ctx context.Context, state extension.SessionState, _ *SessionWhiteboard, args json.RawMessage) (json.RawMessage, error) {
	var in writeInput
	if err := json.Unmarshal(args, &in); err != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid whiteboard:write args: %v", err))
	}
	if in.Text == "" {
		return toolErr("bad_request", "text is required")
	}

	parentState, ok := state.Parent()
	if !ok {
		return toolErr("no_whiteboard_to_write_to",
			"root sessions cannot whiteboard:write; only sub-agents broadcast to their parent's board")
	}
	parentH := FromState(parentState)
	if parentH == nil {
		return toolErr("unavailable", "parent session has no whiteboard handle")
	}
	parentH.mu.Lock()
	hostActive := parentH.wb.Active
	parentH.mu.Unlock()
	if !hostActive {
		return toolErr("no_active_whiteboard",
			"parent session has no active whiteboard to write to")
	}

	role := e.agentID
	payload := writeData{
		FromSessionID: state.SessionID(),
		FromRole:      role,
		Text:          in.Text,
	}
	frame, err := newOpFrame(parentState.SessionID(), state.SessionID(), e.agentParticipant(), OpWrite, payload)
	if err != nil {
		return toolErr("io", err.Error())
	}
	if parentState.IsClosed() {
		return toolErr("io", "host session inbox closed")
	}
	<-parentState.Submit(ctx, frame)
	if parentState.IsClosed() {
		return toolErr("io", "host session inbox closed")
	}
	return json.Marshal(okOutput{OK: true})
}

func (e *Extension) callRead(state extension.SessionState, h *SessionWhiteboard) (json.RawMessage, error) {
	// Per phase-4-spec §7.5: own hosted board takes precedence; if no
	// own active board, fall back to the parent's board the session is
	// a member of.
	source := h
	hostID := state.SessionID()
	source.mu.Lock()
	srcActive := source.wb.Active
	source.mu.Unlock()
	if !srcActive {
		if parentState, ok := state.Parent(); ok {
			if parentH := FromState(parentState); parentH != nil {
				source = parentH
				hostID = parentState.SessionID()
			}
		}
	}
	source.mu.Lock()
	wb := source.wb
	source.mu.Unlock()

	if !wb.Active {
		return json.Marshal(readOutput{Active: false})
	}
	out := readOutput{
		Active:    true,
		HostID:    hostID,
		StartedAt: wb.StartedAt.UTC().Format(time.RFC3339),
		NextSeq:   wb.NextSeq,
	}
	if len(wb.Messages) > 0 {
		out.Messages = make([]readMessageRow, 0, len(wb.Messages))
		for _, m := range wb.Messages {
			out.Messages = append(out.Messages, readMessageRow{
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

func (e *Extension) callStop(ctx context.Context, state extension.SessionState, h *SessionWhiteboard) (json.RawMessage, error) {
	h.mu.Lock()
	active := h.wb.Active
	h.mu.Unlock()
	if !active {
		// Idempotent — stop on a closed board returns ok with no event.
		return json.Marshal(okOutput{OK: true})
	}
	frame, err := newOpFrame(state.SessionID(), "", e.agentParticipant(), OpStop, nil)
	if err != nil {
		return toolErr("io", err.Error())
	}
	if err := state.Emit(ctx, frame); err != nil {
		return toolErr("io", fmt.Sprintf("emit whiteboard:stop: %v", err))
	}
	h.mu.Lock()
	h.wb = Apply(h.wb, ProjectEvent{Op: OpStop, At: frame.OccurredAt()})
	h.mu.Unlock()
	return json.Marshal(okOutput{OK: true})
}

// writeData is the JSON payload of CategoryOp "write" and
// CategoryMessage "message" extension frames. Same shape both ways:
// the host stamps Seq when it persists, and broadcasts inherit it.
type writeData struct {
	Seq           int64  `json:"seq,omitempty"`
	FromSessionID string `json:"from_session_id,omitempty"`
	FromRole      string `json:"from_role,omitempty"`
	Text          string `json:"text"`
	Truncated     bool   `json:"truncated,omitempty"`
}

// newOpFrame builds an [protocol.ExtensionFrame] addressed to
// sessionID, owned by the whiteboard extension. data is marshalled
// to JSON; pass nil for ops without a payload (init / stop). When
// fromSessionID is non-empty, BaseFrame.FromSession is set so the
// host fan-in handler can attribute the write to the source session.
//
// CategoryMessage is selected for OpMessage; everything else uses
// CategoryOp.
func newOpFrame(sessionID, fromSessionID string, author protocol.ParticipantInfo, op string, data any) (*protocol.ExtensionFrame, error) {
	var raw json.RawMessage
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return nil, fmt.Errorf("marshal whiteboard %s data: %w", op, err)
		}
		raw = b
	}
	cat := protocol.CategoryOp
	if op == OpMessage {
		cat = protocol.CategoryMessage
	}
	f := protocol.NewExtensionFrame(sessionID, author, providerName, cat, op, raw)
	if fromSessionID != "" {
		f.BaseFrame.FromSession = fromSessionID
	}
	return f, nil
}
