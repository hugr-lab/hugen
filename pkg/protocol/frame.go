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
	KindUserMessage   Kind = "user_message"
	KindAgentMessage  Kind = "agent_message"
	KindReasoning     Kind = "reasoning"
	KindToolCall      Kind = "tool_call"
	KindToolResult    Kind = "tool_result"
	KindSlashCommand  Kind = "slash_command"
	KindCancel        Kind = "cancel"
	KindSessionOpened Kind = "session_opened"
	KindSessionClosed Kind = "session_closed"
	KindHeartbeat     Kind = "heartbeat"
	KindError         Kind = "error"
	KindSystemMarker  Kind = "system_marker"

	// Phase-4 kinds (sub-agents, runtime injections). Whiteboard and
	// plan state-change events ride [KindExtensionFrame] with
	// Extension="whiteboard" / "plan" instead of dedicated kinds.
	KindSubagentStarted   Kind = "subagent_started"
	KindSubagentResult    Kind = "subagent_result"
	KindSessionTerminated Kind = "session_terminated"
	KindSystemMessage     Kind = "system_message"

	// KindSessionStatus is the persisted lifecycle marker emitted by
	// Session at every state transition (idle ↔ active ↔ wait_*).
	// Newest event in the log is authoritative for restart
	// classification — see [SessionStatusPayload]. Default-deny in
	// the visibility filter; never reaches the model prompt.
	KindSessionStatus Kind = "session_status"

	// Phase-4.1b-pre kind: SessionClose is the internal frame the
	// session loop receives to begin teardown. It is NOT a transcript
	// event — handleExit translates it into the persisted
	// session_terminated row, optionally followed by SessionClosed for
	// adapter back-compat. Producers: Manager.Terminate (root),
	// parent.handleSubagentResult (subagent), parent.teardown (cascade).
	KindSessionClose Kind = "session_close"

	// Phase-5.1 HITL kinds. InquiryRequest bubbles up through the
	// pump chain from any tier's session:inquire tool to root's
	// adapter; InquiryResponse cascades down through ancestors
	// back to the caller (matched by Payload.RequestID). Default-
	// deny in the visibility filter — both kinds are routing
	// envelopes, not model-facing prose. See § 2 of the phase
	// spec.
	KindInquiryRequest  Kind = "inquiry_request"
	KindInquiryResponse Kind = "inquiry_response"
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

	// Envelope additions (phase 4 / `design.md §19 — Foundation 2`).
	// FromSession is filled for cross-session frames; the other two
	// are reserved for phase 5 / phase 10 producers.
	FromSessionID() string
	FromParticipantID() string
	RequestIDValue() string

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
//
// Phase-4 envelope additions (`design.md §19 — Foundation 2`):
//
//   - FromSession is the direct-neighbour sender id and is filled
//     for any Frame that crosses a session boundary in phase 4
//     (subagent_result child→parent, whiteboard_write child→host,
//     whiteboard_message host→members).
//   - FromParticipant is reserved for phase-10 multi-party
//     workspaces; phase 4-9 producers leave it empty.
//   - RequestID is reserved for phase-5 HITL chain forwarding;
//     phase 4 producers leave it empty.
//
// Reserving the slots now means zero schema migration when phase 5
// / phase 10 begin to fill them — every persisted Frame envelope
// already has the field.
type BaseFrame struct {
	ID              string          `json:"frame_id"`
	Session         string          `json:"session_id"`
	K               Kind            `json:"kind"`
	Auth            ParticipantInfo `json:"author"`
	At              time.Time       `json:"occurred_at"`
	S               int             `json:"-"`
	FromSession     string          `json:"from_session,omitempty"`
	FromParticipant string          `json:"from_participant,omitempty"`
	RequestID       string          `json:"request_id,omitempty"`
}

func (b BaseFrame) FrameID() string           { return b.ID }
func (b BaseFrame) SessionID() string         { return b.Session }
func (b BaseFrame) Kind() Kind                { return b.K }
func (b BaseFrame) Author() ParticipantInfo   { return b.Auth }
func (b BaseFrame) OccurredAt() time.Time     { return b.At }
func (b BaseFrame) Seq() int                  { return b.S }
func (b BaseFrame) FromSessionID() string     { return b.FromSession }
func (b BaseFrame) FromParticipantID() string { return b.FromParticipant }
func (b BaseFrame) RequestIDValue() string    { return b.RequestID }

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
	// Final marks the end of the assistant TURN (not iteration) — adapters
	// use it to render newline + prompt cuts. Set on the consolidated
	// row for the model iteration that retires the turn (i.e. produced
	// no tool calls). Tool-call iterations retire later, after the
	// dispatcher's results land.
	Final bool `json:"final"`
	// Consolidated marks the per-iteration "complete assistant record"
	// row that lands in the persisted event log: assembled text + tool
	// calls + reasoning state for ONE model iteration. Live streaming
	// chunks are emitted with Consolidated=false and stay outbox-only
	// (adapters render them incrementally; never persisted). Replay
	// reads only Consolidated=true rows so model.Message history
	// reconstructs exactly the per-iteration assistant turns the runtime
	// originally appended — no chunk reassembly, no orphan tool_calls.
	Consolidated bool `json:"consolidated,omitempty"`
	// Consolidated-only fields. Carry the assistant iteration's tool
	// calls + reasoning state alongside the assembled text.
	ToolCalls        []ToolCallPayload `json:"tool_calls,omitempty"`
	Thinking         string            `json:"thinking,omitempty"`
	ThoughtSignature string            `json:"thought_signature,omitempty"`
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
	// Cascade controls whether the cancel terminates the receiving
	// session's sub-agent subtree in addition to aborting the
	// in-flight turn.
	//
	//   - false (default) → only the receiving session's in-flight
	//     turn stops (mdl.Generate + any in-flight tool dispatches
	//     abort). Sub-agents keep running; the model reacts on its
	//     next prompt build.
	//   - true → same in-flight abort PLUS the receiving session
	//     calls Manager.Terminate(child, "cancel_cascade") for every
	//     active child. Each child's ctx-cancellation propagates
	//     down its subtree; each writes its own
	//     session_terminated{reason:"cancel_cascade"} event. The
	//     receiving session itself does NOT terminate.
	//
	// User-facing slash commands: /cancel → false; /cancel all → true.
	Cascade bool `json:"cascade,omitempty"`
}

type SessionOpenedPayload struct {
	Participants []ParticipantInfo `json:"participants"`
}

type SessionClosedPayload struct {
	Reason string `json:"reason"`
}

// SessionClosePayload is the trigger Frame for the session goroutine
// to begin teardown. Reason is the verbatim string written into the
// persisted session_terminated event when the Run loop exits in
// response. SessionClose is internal control plane: producers are
// Manager.Terminate, parent.handleSubagentResult, parent.teardown,
// and self-close paths via Session.requestClose.
type SessionClosePayload struct {
	Reason string `json:"reason"`
}

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

	// skill:save typed codes (phase 4.2). These give the LLM
	// distinct signals it can map to specific recovery flows
	// instead of treating every tool error as generic IO. The
	// `_skill_builder` body documents the right action per code:
	//   - skill_exists       → ask the user before overwrite
	//   - skill_bad_manifest → fix manifest and re-save
	//   - skill_bad_path     → use simple bundle keys
	//   - skill_autoload     → drop autoload from manifest
	ToolErrorSkillExists      = "skill_exists"
	ToolErrorSkillBadManifest = "skill_bad_manifest"
	ToolErrorSkillBadPath     = "skill_bad_path"
	ToolErrorSkillAutoload    = "skill_autoload"
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

// Phase-4 payloads.

// SubagentStartedPayload is appended to the parent's events when a
// child sub-agent session is spawned. Inputs is arbitrary JSON the
// parent passes to the child via spawn_subagent.
type SubagentStartedPayload struct {
	ChildSessionID string    `json:"child_session_id"`
	Skill          string    `json:"skill,omitempty"`
	Role           string    `json:"role,omitempty"`
	Task           string    `json:"task"`
	Depth          int       `json:"depth"`
	StartedAt      time.Time `json:"started_at"`
	Inputs         any       `json:"inputs,omitempty"`
}

// SubagentResultPayload is delivered to the parent's inbox as a Frame
// when the sub-agent's goroutine exits. Reason mirrors the child's
// session_terminated.reason exactly: "completed" | "hard_ceiling" |
// "subagent_cancel: <rationale>" | "cancel_cascade" | "restart_died" |
// "panic: <msg>" | ...
type SubagentResultPayload struct {
	SessionID string `json:"session_id"`
	Result    string `json:"result,omitempty"`
	Reason    string `json:"reason"`
	TurnsUsed int    `json:"turns_used"`
	// Goal mirrors the child's original mission text (truncated by
	// the pump). Carried alongside the result so async completion
	// renders (Phase 5.1 § 4.3) can quote the mission goal without
	// re-querying state.
	Goal string `json:"goal,omitempty"`
	// RenderMode selects the visibility-projection template for
	// this result. Empty / "default" → "[system: subagent_result]
	// …" (the standard wait_subagents path). "async_notify" →
	// "interrupts/async_mission_completed.tmpl" (Phase 5.1 § 4.3).
	// "silent" → skip history projection entirely (the event is
	// still persisted). Set by spawn_mission's async / timeout
	// branches at spawn time and read by the parent pump when
	// constructing the result.
	RenderMode string `json:"render_mode,omitempty"`
}

// Subagent render modes — see [SubagentResultPayload.RenderMode].
const (
	SubagentRenderDefault     = ""
	SubagentRenderAsyncNotify = "async_notify"
	SubagentRenderSilent      = "silent"
)

// SessionTerminatedPayload is the sole terminal write for any
// session. Reason is free-form; phase-4 writers use:
// "completed", "hard_ceiling", "subagent_cancel: <rationale>",
// "cancel_cascade", "restart_died", "panic: <msg>", "user:/end".
type SessionTerminatedPayload struct {
	Reason    string `json:"reason"`
	Result    string `json:"result,omitempty"`
	TurnsUsed int    `json:"turns_used,omitempty"`
}

// SessionStatus state values mark the session's lifecycle stage in
// its own events log. Idle = quiescent (turn closed, no live work).
// Active = a turn is in progress. The wait_* values mark explicit
// runtime pauses — wait_subagents during spawn-wait, wait_approval
// / wait_user_input during a session:inquire call via
// ToolFeed.BlockingState.
const (
	SessionStatusIdle          = "idle"
	SessionStatusActive        = "active"
	SessionStatusWaitSubagents = "wait_subagents"
	SessionStatusWaitApproval  = "wait_approval"
	SessionStatusWaitUserInput = "wait_user_input"
)

// SessionStatusPayload is the lifecycle marker the Session emits at
// every state transition. The NEWEST KindSessionStatus event in a
// session's log is authoritative for restart classification —
// Manager.RestoreActive reads it to decide whether to re-attach a
// goroutine eagerly (active / wait_*) or leave the session dormant
// until an adapter Resume (idle).
//
// State is one of the SessionStatus* constants. Reason is a short
// free-form trigger label ("user_message", "wait_subagents tool",
// "turn quiescent", …) used purely for diagnostics and the audit
// log; the runtime never branches on it.
//
// Phase 5.1b grew the payload with three optional snapshots of
// the session's live state at emission time, so adapters that
// render rich status panes (TUI, future SSE / A2A) don't have to
// walk the event log to recover them. All three fields are
// `omitempty` — adapters that only read State / Reason continue
// to work unchanged.
type SessionStatusPayload struct {
	State           string             `json:"state"`
	Reason          string             `json:"reason,omitempty"`
	ActiveSubagents []ActiveSubagentRef `json:"active_subagents,omitempty"`
	PendingInquiry  *PendingInquiryRef  `json:"pending_inquiry,omitempty"`
	LastToolCall    *ToolCallRef        `json:"last_tool_call,omitempty"`
}

// ActiveSubagentRef is one entry in SessionStatusPayload.ActiveSubagents.
// Populated when the emitting session has live children at the
// time of the status transition.
type ActiveSubagentRef struct {
	SessionID string    `json:"session_id"`
	Skill     string    `json:"skill,omitempty"`
	Role      string    `json:"role,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

// PendingInquiryRef snapshots the in-flight session:inquire when
// the session emits its KindSessionStatus(wait_approval) or
// KindSessionStatus(wait_user_input) event. Nil when the session
// is not blocked on an inquiry.
type PendingInquiryRef struct {
	RequestID string    `json:"request_id"`
	Type      string    `json:"type"` // approval | clarification
	Question  string    `json:"question"`
	StartedAt time.Time `json:"started_at"`
}

// ToolCallRef snapshots the most recently dispatched tool at the
// time of a status transition. Used by adapters to render
// "running: hugr-main:discovery-search_data_sources" indicators.
type ToolCallRef struct {
	Name      string    `json:"name"`
	StartedAt time.Time `json:"started_at"`
}

// SystemMessagePayload is a model-visible runtime injection (distinct
// from the UI-only SystemMarker). Rendered into the session's
// in-memory model history as model.Message{Role:RoleUser,
// Content:"[system: <kind>] <content>"} (provider-portable across
// Anthropic / OpenAI / Gemini).
//
// Phase-4 Kind values: "soft_warning", "stuck_nudge", "whiteboard",
// "spawned_note".
type SystemMessagePayload struct {
	Kind    string `json:"kind"`
	Content string `json:"content"`
}

// Phase-4 system_message kinds.
const (
	SystemMessageSoftWarning = "soft_warning"
	SystemMessageStuckNudge  = "stuck_nudge"
	SystemMessageWhiteboard  = "whiteboard"
	SystemMessageSpawnedNote = "spawned_note"
	// SystemMessageParentNote is the kind for a notify_subagent
	// directive. Phase 5.1 § 3.4 / § 3.5: the directive rides an
	// ordinary KindSystemMessage with FromSession == parent.id so
	// wait_subagents can intercept it via the isParentNote predicate
	// and the visibility filter projects "[system: parent_note] ..."
	// into the child's history when not under wait_subagents.
	SystemMessageParentNote = "parent_note"
)

// Phase-4 system_marker subjects (machine-readable, adapter-only).
const (
	SubjectMCPRecovered    = "mcp_recovered"
	SubjectHardCeilingHit  = "hard_ceiling_hit"
	SubjectNoProgress      = "no_progress"
)

// Phase-4 session_terminated reason constants. Reason is free-form;
// these are the well-known values phase-4 producers emit.
const (
	TerminationCompleted      = "completed"
	TerminationHardCeiling    = "hard_ceiling"
	TerminationCancelCascade  = "cancel_cascade"
	TerminationRestartDied    = "restart_died"
	TerminationUserEnd        = "user:/end"
	// TerminationSubagentCancelPrefix is concatenated with the
	// caller-provided rationale: "subagent_cancel: <rationale>".
	TerminationSubagentCancelPrefix = "subagent_cancel: "
	// TerminationPanicPrefix is concatenated with the recovered
	// panic message: "panic: <msg>".
	TerminationPanicPrefix = "panic: "
)

// Concrete phase-4 Frame variants.

type SubagentStarted struct {
	BaseFrame
	Payload SubagentStartedPayload
}

type SubagentResult struct {
	BaseFrame
	Payload SubagentResultPayload
}

type SessionTerminated struct {
	BaseFrame
	Payload SessionTerminatedPayload
}

// SessionClose is the trigger Frame for session teardown. See
// SessionClosePayload. The receiving session's Run loop translates
// SessionClose into a persisted session_terminated row.
type SessionClose struct {
	BaseFrame
	Payload SessionClosePayload
}

type SystemMessage struct {
	BaseFrame
	Payload SystemMessagePayload
}

// SessionStatus is the lifecycle marker frame. See
// [SessionStatusPayload].
type SessionStatus struct {
	BaseFrame
	Payload SessionStatusPayload
}

// InquiryRequestPayload is the bubble-up envelope a tier's
// session:inquire tool emits. RequestID drives response routing
// (cascade-down via parent-mediated responseRouting maps); the
// caller's session id is preserved in CallerSessionID for the
// adapter to address its eventual InquiryResponse. SessionID on
// the embedding BaseFrame is rewritten on each hop by the pump
// so Runtime.fanout keys correctly when the frame reaches root.
// Phase 5.1 § 2.2.
type InquiryRequestPayload struct {
	RequestID       string   `json:"request_id"`
	CallerSessionID string   `json:"caller_session_id"`
	Type            string   `json:"type"` // approval | clarification
	Question        string   `json:"question"`
	Context         string   `json:"context,omitempty"`
	Options         []string `json:"options,omitempty"`
	TimeoutMs       int      `json:"timeout_ms,omitempty"`
	CreatedAt       string   `json:"created_at,omitempty"`
}

// InquiryResponsePayload is the cascade-down answer. The adapter
// addresses it to the root session id (where it received the
// request); root's internal handler forwards it down the parent
// chain via responseRouting until CallerSessionID == s.id at the
// caller's hop. Approved/Reason for approval; Response/RespondedAt
// for clarification; Timeout for runtime-synthesised expiries.
// Phase 5.1 § 2.2.
type InquiryResponsePayload struct {
	RequestID       string `json:"request_id"`
	CallerSessionID string `json:"caller_session_id"`
	Approved        *bool  `json:"approved,omitempty"`
	Reason          string `json:"reason,omitempty"`
	Response        string `json:"response,omitempty"`
	RespondedAt     string `json:"responded_at,omitempty"`
	Timeout         bool   `json:"timeout,omitempty"`
}

// Phase-5.1 inquiry types — values for InquiryRequestPayload.Type.
const (
	InquiryTypeApproval      = "approval"
	InquiryTypeClarification = "clarification"
)

// InquiryRequest is the frame that bubbles a [session:inquire]
// call up the parent chain to root, where the adapter surfaces
// it to the user. Phase 5.1 § 2.
type InquiryRequest struct {
	BaseFrame
	Payload InquiryRequestPayload
}

// InquiryResponse is the cascade-down answer carried back to
// the caller through responseRouting tables on each ancestor.
// Phase 5.1 § 2.
type InquiryResponse struct {
	BaseFrame
	Payload InquiryResponsePayload
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

func (f UserMessage) payload() any       { return f.Payload }
func (f AgentMessage) payload() any      { return f.Payload }
func (f Reasoning) payload() any         { return f.Payload }
func (f ToolCall) payload() any          { return f.Payload }
func (f ToolResult) payload() any        { return f.Payload }
func (f SlashCommand) payload() any      { return f.Payload }
func (f Cancel) payload() any            { return f.Payload }
func (f SessionOpened) payload() any     { return f.Payload }
func (f SessionClosed) payload() any     { return f.Payload }
func (f Heartbeat) payload() any         { return f.Payload }
func (f Error) payload() any             { return f.Payload }
func (f SystemMarker) payload() any      { return f.Payload }
func (f SubagentStarted) payload() any   { return f.Payload }
func (f SubagentResult) payload() any    { return f.Payload }
func (f SessionTerminated) payload() any { return f.Payload }
func (f SessionClose) payload() any      { return f.Payload }
func (f SystemMessage) payload() any     { return f.Payload }
func (f SessionStatus) payload() any     { return f.Payload }
func (f InquiryRequest) payload() any    { return f.Payload }
func (f InquiryResponse) payload() any   { return f.Payload }
func (f OpaqueFrame) payload() any       { return f.RawPayload }

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

// NewAgentMessageConsolidated constructs the per-iteration assistant
// turn record: full assembled text + tool calls + reasoning state.
// Consolidated=true is implied. Final reflects whether this iteration
// retires the assistant turn — true when there are no tool calls to
// dispatch (next iteration would be a fresh user input, not another
// model call), false when tool calls follow. Emitted once per model
// iteration by foldAssistantAndMaybeDispatch; this is the only
// AgentMessage shape that lands in the persisted event log.
func NewAgentMessageConsolidated(sessionID string, author ParticipantInfo, text string, seq int, final bool,
	toolCalls []ToolCallPayload, thinking, thoughtSignature string) *AgentMessage {
	return &AgentMessage{
		BaseFrame: newBase(sessionID, KindAgentMessage, author),
		Payload: AgentMessagePayload{
			Text:             text,
			ChunkSeq:         seq,
			Final:            final,
			Consolidated:     true,
			ToolCalls:        toolCalls,
			Thinking:         thinking,
			ThoughtSignature: thoughtSignature,
		},
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

// Phase-4 constructors.

func NewSubagentStarted(parentSessionID string, author ParticipantInfo, p SubagentStartedPayload) *SubagentStarted {
	if p.StartedAt.IsZero() {
		p.StartedAt = time.Now().UTC()
	}
	return &SubagentStarted{
		BaseFrame: newBase(parentSessionID, KindSubagentStarted, author),
		Payload:   p,
	}
}

// NewSubagentResult builds the cross-session terminal result Frame.
// fromSessionID is the child's session id; the BaseFrame.FromSession
// envelope field is filled to match.
func NewSubagentResult(parentSessionID, fromSessionID string, author ParticipantInfo, p SubagentResultPayload) *SubagentResult {
	base := newBase(parentSessionID, KindSubagentResult, author)
	base.FromSession = fromSessionID
	if p.SessionID == "" {
		p.SessionID = fromSessionID
	}
	return &SubagentResult{BaseFrame: base, Payload: p}
}

func NewSessionTerminated(sessionID string, author ParticipantInfo, p SessionTerminatedPayload) *SessionTerminated {
	return &SessionTerminated{
		BaseFrame: newBase(sessionID, KindSessionTerminated, author),
		Payload:   p,
	}
}

// NewSessionClose builds the internal teardown trigger Frame.
// Producers: Manager.Terminate, parent.handleSubagentResult,
// parent.teardown (cascade). The receiving session's Run loop
// translates this into a persisted session_terminated row with the
// supplied reason and exits.
func NewSessionClose(sessionID string, author ParticipantInfo, reason string) *SessionClose {
	return &SessionClose{
		BaseFrame: newBase(sessionID, KindSessionClose, author),
		Payload:   SessionClosePayload{Reason: reason},
	}
}

func NewSystemMessage(sessionID string, author ParticipantInfo, kind, content string) *SystemMessage {
	return &SystemMessage{
		BaseFrame: newBase(sessionID, KindSystemMessage, author),
		Payload:   SystemMessagePayload{Kind: kind, Content: content},
	}
}

// NewInquiryRequest builds the bubble-up envelope a session's
// inquire tool emits. sessionID is the caller's id at emit time;
// pump rewrites SessionID on each hop while CallerSessionID
// (preserved in the payload) carries the originator end-to-end.
// Phase 5.1 § 2.
func NewInquiryRequest(sessionID string, author ParticipantInfo, p InquiryRequestPayload) *InquiryRequest {
	if p.CallerSessionID == "" {
		p.CallerSessionID = sessionID
	}
	if p.CreatedAt == "" {
		p.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	return &InquiryRequest{
		BaseFrame: newBase(sessionID, KindInquiryRequest, author),
		Payload:   p,
	}
}

// NewInquiryResponse builds the cascade-down answer the adapter
// hands to runtime.Submit. The adapter addresses it to the root
// session id (the same id it received the request on); ancestors
// forward it down the chain until CallerSessionID == s.id.
// Phase 5.1 § 2.
func NewInquiryResponse(sessionID string, author ParticipantInfo, p InquiryResponsePayload) *InquiryResponse {
	return &InquiryResponse{
		BaseFrame: newBase(sessionID, KindInquiryResponse, author),
		Payload:   p,
	}
}

// NewSessionStatus builds a lifecycle marker frame for state with an
// optional reason label. Author is the agent's participant info —
// the runtime stamps its own status, not a user.
func NewSessionStatus(sessionID string, author ParticipantInfo, state, reason string) *SessionStatus {
	return &SessionStatus{
		BaseFrame: newBase(sessionID, KindSessionStatus, author),
		Payload:   SessionStatusPayload{State: state, Reason: reason},
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
	case *SubagentStarted:
		if v.Payload.ChildSessionID == "" {
			return fmt.Errorf("protocol: subagent_started missing child_session_id")
		}
		if v.Payload.Task == "" {
			return fmt.Errorf("protocol: subagent_started missing task")
		}
	case *SubagentResult:
		if v.Payload.SessionID == "" {
			return fmt.Errorf("protocol: subagent_result missing session_id")
		}
		if v.Payload.Reason == "" {
			return fmt.Errorf("protocol: subagent_result missing reason")
		}
	case *SessionTerminated:
		if v.Payload.Reason == "" {
			return fmt.Errorf("protocol: session_terminated missing reason")
		}
	case *SessionClose:
		if v.Payload.Reason == "" {
			return fmt.Errorf("protocol: session_close missing reason")
		}
	case *SystemMessage:
		if v.Payload.Kind == "" {
			return fmt.Errorf("protocol: system_message missing kind")
		}
	case *SessionStatus:
		switch v.Payload.State {
		case SessionStatusIdle, SessionStatusActive,
			SessionStatusWaitSubagents, SessionStatusWaitApproval,
			SessionStatusWaitUserInput:
		default:
			return fmt.Errorf("protocol: session_status invalid state %q", v.Payload.State)
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
