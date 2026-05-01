// Package protocol defines the Frame tagged-union that the hugen
// runtime exchanges with adapters and persists in session_events.
//
// Every Frame embeds [BaseFrame] and carries a discriminator [Kind].
// New<Variant> constructors fill FrameID (UUID v7) and OccurredAt
// (UTC) when zero, so callers can pass a partial struct.
package protocol

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Kind discriminates Frame variants. Phase-1 set is closed; later
// phases add new kinds without breaking existing decoders.
type Kind string

const (
	KindUserMessage      Kind = "user_message"
	KindAgentMessage     Kind = "agent_message"
	KindReasoning        Kind = "reasoning"
	KindToolCall         Kind = "tool_call"
	KindToolResult       Kind = "tool_result"
	KindSlashCommand     Kind = "slash_command"
	KindCancel           Kind = "cancel"
	KindSessionOpened    Kind = "session_opened"
	KindSessionClosed    Kind = "session_closed"
	KindSessionSuspended Kind = "session_suspended"
	KindHeartbeat        Kind = "heartbeat"
	KindError            Kind = "error"
	KindSystemMarker     Kind = "system_marker"
)

// ParticipantInfo identifies who emitted (or is addressed by) a Frame.
type ParticipantInfo struct {
	ID    string   `json:"id"`
	Kind  string   `json:"kind"` // user | agent | system
	Name  string   `json:"name,omitempty"`
	Roles []string `json:"roles,omitempty"`
}

const (
	ParticipantUser   = "user"
	ParticipantAgent  = "agent"
	ParticipantSystem = "system"
)

// Frame is the closed tagged union. Every concrete variant embeds
// BaseFrame and reports its discriminator via Kind().
//
// Seq() reports the per-session strictly-monotonic sequence number
// the runtime assigned when persisting the frame. Live frames flowing
// through Session.emit carry their assigned seq from the moment of
// AppendEvent; frames materialised from the store (replay) carry
// their persisted seq. A non-zero seq means the frame has been
// committed to the event log; zero means the frame has not been
// persisted yet (constructed by a new<Variant> call but not yet
// emitted). The HTTP adapter uses Seq() for the SSE `id:` line and
// for replay/live dedupe (R-Plan-16).
type Frame interface {
	FrameID() string
	SessionID() string
	Kind() Kind
	Author() ParticipantInfo
	OccurredAt() time.Time
	Seq() int

	// payload returns the variant payload as a JSON-encodable value.
	// The codec uses this to produce the wire payload object.
	payload() any
}

// SeqSetter is implemented by Frame variants whose seq is filled in
// after construction (every variant that embeds BaseFrame). The
// runtime calls SetSeq once, after AppendEvent assigns the cursor.
// Adapters never call SetSeq.
type SeqSetter interface {
	SetSeq(int)
}

// BaseFrame holds envelope fields shared by every variant.
//
// S is the per-session sequence number — zero until the runtime
// persists the frame, at which point Session.emit calls SetSeq via
// the SeqSetter interface. The field is private-ish (lowercase
// JSON tag, no JSON serialisation) because the wire envelope uses
// the SSE `id:` line, not the JSON payload, to carry seq.
type BaseFrame struct {
	ID      string          `json:"frame_id"`
	Session string          `json:"session_id"`
	K       Kind            `json:"kind"`
	Auth    ParticipantInfo `json:"author"`
	At      time.Time       `json:"occurred_at"`
	S       int             `json:"-"`
}

func (b BaseFrame) FrameID() string         { return b.ID }
func (b BaseFrame) SessionID() string       { return b.Session }
func (b BaseFrame) Kind() Kind              { return b.K }
func (b BaseFrame) Author() ParticipantInfo { return b.Auth }
func (b BaseFrame) OccurredAt() time.Time   { return b.At }
func (b BaseFrame) Seq() int                { return b.S }

// SetSeq sets the per-session sequence number. The runtime calls it
// once, after AppendEvent assigns the cursor; adapters never call
// it. Pointer receiver so the method propagates to every concrete
// variant pointer (every constructor returns a pointer). Variants
// satisfy SeqSetter automatically through embedding.
func (b *BaseFrame) SetSeq(s int) { b.S = s }

// Variant payloads.

type UserMessagePayload struct {
	Text string `json:"text"`
}

type AgentMessagePayload struct {
	Text     string `json:"text"`
	ChunkSeq int    `json:"chunk_seq"`
	Final    bool   `json:"final"`
}

type ReasoningPayload struct {
	Text     string `json:"text"`
	ChunkSeq int    `json:"chunk_seq"`
	Final    bool   `json:"final"`
}

type ToolCallPayload struct {
	ToolID string `json:"tool_id"`
	Name   string `json:"name"`
	Args   any    `json:"args,omitempty"`
}

type ToolResultPayload struct {
	ToolID  string `json:"tool_id"`
	Result  any    `json:"result,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
}

type SlashCommandPayload struct {
	Name string   `json:"name"`
	Args []string `json:"args"`
	Raw  string   `json:"raw"`
}

type CancelPayload struct {
	Reason string `json:"reason"`
}

type SessionOpenedPayload struct {
	Participants []ParticipantInfo `json:"participants"`
}

type SessionClosedPayload struct {
	Reason string `json:"reason"`
}

type SessionSuspendedPayload struct{}

type HeartbeatPayload struct {
	Now time.Time `json:"now"`
}

type ErrorPayload struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Recoverable bool   `json:"recoverable,omitempty"`
}

type SystemMarkerPayload struct {
	Subject string         `json:"subject"`
	Details map[string]any `json:"details,omitempty"`
}

// Phase-3 system_marker subjects. Adapters surface these as
// audit events in the conversation transcript; the runtime emits
// them at significant lifecycle steps so an operator can
// reconstruct what happened post-hoc.
const (
	SubjectToolDenied            = "tool_denied"
	SubjectPermissionRefresh     = "permission_refresh"
	SubjectSkillLoaded           = "skill_loaded"
	SubjectSkillUnloaded         = "skill_unloaded"
	SubjectSkillPublished        = "skill_published"
	SubjectSkillPublishBlocked   = "skill_publish_blocked"
	SubjectSkillDependencyFailed = "skill_dependency_failed"
	SubjectMCPProviderAdded      = "mcp_provider_added"
	SubjectMCPProviderRemoved    = "mcp_provider_removed"
	SubjectMCPProviderCrashed    = "mcp_provider_crashed"
)

// Phase-3 tool_error codes. Returned as the "code" field inside
// a ToolResult{IsError:true}.Result map (or a typed ToolError
// payload — wire-compatible). Tools that abort dispatch use these
// codes so the LLM can react predictably and audit can categorise.
const (
	ToolErrorPermissionDenied = "permission_denied"
	ToolErrorTimeout          = "timeout"
	ToolErrorPathEscape       = "path_escape"
	ToolErrorReadOnly         = "readonly"
	ToolErrorNotFound         = "not_found"
	ToolErrorProviderCrashed  = "provider_crashed"
	ToolErrorProviderRemoved  = "provider_removed"
	ToolErrorOutputTruncated  = "output_truncated"
	ToolErrorHugrError        = "hugr_error"
	ToolErrorAuth             = "auth"
	ToolErrorIO               = "io"
	ToolErrorJQError          = "jq_error"
	ToolErrorArgValidation    = "arg_validation"
)

// ToolError is the recommended structured shape callers stuff
// into ToolResultPayload.Result when IsError is true. It is not
// load-bearing for adapters — they treat Result as opaque — but
// using it in the runtime keeps audit and rendering consistent.
type ToolError struct {
	Code      string         `json:"code"`
	Message   string         `json:"message,omitempty"`
	Tier      string         `json:"tier,omitempty"`       // "config" | "remote" | "user" — for permission_denied
	ElapsedMs int            `json:"elapsed_ms,omitempty"` // for timeout
	Details   map[string]any `json:"details,omitempty"`
}

// Concrete variants. Each embeds BaseFrame + a typed payload.

type UserMessage struct {
	BaseFrame
	Payload UserMessagePayload
}

type AgentMessage struct {
	BaseFrame
	Payload AgentMessagePayload
}

type Reasoning struct {
	BaseFrame
	Payload ReasoningPayload
}

type ToolCall struct {
	BaseFrame
	Payload ToolCallPayload
}

type ToolResult struct {
	BaseFrame
	Payload ToolResultPayload
}

type SlashCommand struct {
	BaseFrame
	Payload SlashCommandPayload
}

type Cancel struct {
	BaseFrame
	Payload CancelPayload
}

type SessionOpened struct {
	BaseFrame
	Payload SessionOpenedPayload
}

type SessionClosed struct {
	BaseFrame
	Payload SessionClosedPayload
}

type SessionSuspended struct {
	BaseFrame
	Payload SessionSuspendedPayload
}

type Heartbeat struct {
	BaseFrame
	Payload HeartbeatPayload
}

type Error struct {
	BaseFrame
	Payload ErrorPayload
}

type SystemMarker struct {
	BaseFrame
	Payload SystemMarkerPayload
}

// OpaqueFrame represents a Frame variant the codec does not know.
// Phase 2 introduces opaque round-trip so future-phase variants
// (sub_agent_*, approval_*, clarification_*, ...) survive an
// encode→decode→encode trip even when the running binary doesn't
// act on them.
//
// Only the codec materialises *OpaqueFrame (via newOpaqueFrame);
// runtime / adapter code never branches on it. The closed union for
// consumers stays closed. See
// specs/002-agent-runtime-phase-2/contracts/sse-wire-format.md
// §"Variants on the wire".
type OpaqueFrame struct {
	BaseFrame
	KindRaw    string
	RawPayload json.RawMessage
}

func (f UserMessage) payload() any      { return f.Payload }
func (f AgentMessage) payload() any     { return f.Payload }
func (f Reasoning) payload() any        { return f.Payload }
func (f ToolCall) payload() any         { return f.Payload }
func (f ToolResult) payload() any       { return f.Payload }
func (f SlashCommand) payload() any     { return f.Payload }
func (f Cancel) payload() any           { return f.Payload }
func (f SessionOpened) payload() any    { return f.Payload }
func (f SessionClosed) payload() any    { return f.Payload }
func (f SessionSuspended) payload() any { return f.Payload }
func (f Heartbeat) payload() any        { return f.Payload }
func (f Error) payload() any            { return f.Payload }
func (f SystemMarker) payload() any     { return f.Payload }
func (f OpaqueFrame) payload() any      { return f.RawPayload }

// newOpaqueFrame is package-private so only the codec materialises
// opaque frames. base.K MUST equal kindRaw at the call site.
func newOpaqueFrame(base BaseFrame, kindRaw string, rawPayload json.RawMessage) *OpaqueFrame {
	if len(rawPayload) == 0 {
		rawPayload = json.RawMessage("{}")
	}
	return &OpaqueFrame{BaseFrame: base, KindRaw: kindRaw, RawPayload: rawPayload}
}

// Constructors fill defaults so callers can pass partial structs.

func newBase(sessionID string, kind Kind, author ParticipantInfo) BaseFrame {
	return BaseFrame{
		ID:      newFrameID(),
		Session: sessionID,
		K:       kind,
		Auth:    author,
		At:      time.Now().UTC(),
	}
}

func NewUserMessage(sessionID string, author ParticipantInfo, text string) *UserMessage {
	return &UserMessage{
		BaseFrame: newBase(sessionID, KindUserMessage, author),
		Payload:   UserMessagePayload{Text: text},
	}
}

func NewAgentMessage(sessionID string, author ParticipantInfo, text string, seq int, final bool) *AgentMessage {
	return &AgentMessage{
		BaseFrame: newBase(sessionID, KindAgentMessage, author),
		Payload:   AgentMessagePayload{Text: text, ChunkSeq: seq, Final: final},
	}
}

func NewReasoning(sessionID string, author ParticipantInfo, text string, seq int, final bool) *Reasoning {
	return &Reasoning{
		BaseFrame: newBase(sessionID, KindReasoning, author),
		Payload:   ReasoningPayload{Text: text, ChunkSeq: seq, Final: final},
	}
}

func NewToolCall(sessionID string, author ParticipantInfo, toolID, name string, args any) *ToolCall {
	return &ToolCall{
		BaseFrame: newBase(sessionID, KindToolCall, author),
		Payload:   ToolCallPayload{ToolID: toolID, Name: name, Args: args},
	}
}

func NewToolResult(sessionID string, author ParticipantInfo, toolID string, result any, isError bool) *ToolResult {
	return &ToolResult{
		BaseFrame: newBase(sessionID, KindToolResult, author),
		Payload:   ToolResultPayload{ToolID: toolID, Result: result, IsError: isError},
	}
}

func NewSlashCommand(sessionID string, author ParticipantInfo, name string, args []string, raw string) *SlashCommand {
	return &SlashCommand{
		BaseFrame: newBase(sessionID, KindSlashCommand, author),
		Payload:   SlashCommandPayload{Name: name, Args: args, Raw: raw},
	}
}

func NewCancel(sessionID string, author ParticipantInfo, reason string) *Cancel {
	if reason == "" {
		reason = "user_cancelled"
	}
	return &Cancel{
		BaseFrame: newBase(sessionID, KindCancel, author),
		Payload:   CancelPayload{Reason: reason},
	}
}

func NewSessionOpened(sessionID string, author ParticipantInfo, parts []ParticipantInfo) *SessionOpened {
	return &SessionOpened{
		BaseFrame: newBase(sessionID, KindSessionOpened, author),
		Payload:   SessionOpenedPayload{Participants: parts},
	}
}

func NewSessionClosed(sessionID string, author ParticipantInfo, reason string) *SessionClosed {
	return &SessionClosed{
		BaseFrame: newBase(sessionID, KindSessionClosed, author),
		Payload:   SessionClosedPayload{Reason: reason},
	}
}

func NewSessionSuspended(sessionID string, author ParticipantInfo) *SessionSuspended {
	return &SessionSuspended{
		BaseFrame: newBase(sessionID, KindSessionSuspended, author),
	}
}

func NewHeartbeat(sessionID string, author ParticipantInfo) *Heartbeat {
	return &Heartbeat{
		BaseFrame: newBase(sessionID, KindHeartbeat, author),
		Payload:   HeartbeatPayload{Now: time.Now().UTC()},
	}
}

func NewError(sessionID string, author ParticipantInfo, code, msg string, recoverable bool) *Error {
	return &Error{
		BaseFrame: newBase(sessionID, KindError, author),
		Payload:   ErrorPayload{Code: code, Message: msg, Recoverable: recoverable},
	}
}

func NewSystemMarker(sessionID string, author ParticipantInfo, subject string, details map[string]any) *SystemMarker {
	return &SystemMarker{
		BaseFrame: newBase(sessionID, KindSystemMarker, author),
		Payload:   SystemMarkerPayload{Subject: subject, Details: details},
	}
}

// Validate returns a non-nil error if a Frame would be rejected by
// the codec. Constructors don't validate; callers building frames
// from external input should run Validate.
func Validate(f Frame) error {
	if f == nil {
		return fmt.Errorf("protocol: nil frame")
	}
	if f.SessionID() == "" {
		return fmt.Errorf("protocol: empty session_id (kind=%s)", f.Kind())
	}
	if f.FrameID() == "" {
		return fmt.Errorf("protocol: empty frame_id (kind=%s)", f.Kind())
	}
	if f.Kind() == "" {
		return fmt.Errorf("protocol: empty kind")
	}
	if f.OccurredAt().IsZero() {
		return fmt.Errorf("protocol: zero occurred_at (kind=%s)", f.Kind())
	}
	a := f.Author()
	if a.ID == "" || a.Kind == "" {
		return fmt.Errorf("protocol: invalid author (kind=%s)", f.Kind())
	}
	switch v := f.(type) {
	case *AgentMessage:
		// Empty text on a final agent_message is valid — it acts as an
		// end-of-turn marker when the provider streamed the content
		// in earlier non-final chunks.
		_ = v
	case *SlashCommand:
		if v.Payload.Name == "" {
			return fmt.Errorf("protocol: empty slash command name")
		}
	case *OpaqueFrame:
		// OpaqueFrame round-trips deferred kinds the binary doesn't
		// recognise. The BaseFrame checks above already enforce a
		// non-empty kind / session / author / timestamp; the payload
		// is opaque by design and not validated.
	}
	return nil
}

// newFrameID returns a 32-char hex random id (UUID-like, sufficient
// for primary-key uniqueness; we don't need the temporal ordering
// guarantees of UUIDv7 for phase 1 — created_at carries that).
func newFrameID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is exceptional; fall back to time-only.
		return fmt.Sprintf("frame-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
