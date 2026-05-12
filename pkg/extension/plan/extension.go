package plan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/prompts"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// StateKey is the [extension.SessionState] key the extension stores
// its per-session [*SessionPlan] handle under. Exported so callers
// outside the extension can recover the handle without magic
// strings.
const StateKey = "plan"

// providerName is the catalogue prefix the LLM sees: "plan:<tool>".
// Doubles as [Extension.Name] and (transitively) the [StateKey].
const providerName = "plan"

// Permission objects the runtime gates the plan tools on. Mirrored
// verbatim from the legacy session: tool entries so existing config
// keeps working.
const (
	PermObjectWrite = "hugen:plan:write"
	PermObjectRead  = "hugen:plan:read"
)

// Extension wires the plan tool surface + per-session projection
// into the session capability pipeline. The instance is shared
// across every session under one Manager; per-session state lives
// in [extension.SessionState] under [StateKey].
type Extension struct {
	agentID string
}

// NewExtension constructs the plan extension. agentID stamps the
// ParticipantInfo on every emitted plan extension_frame.
func NewExtension(agentID string) *Extension {
	return &Extension{agentID: agentID}
}

// Compile-time interface assertions.
var (
	_ extension.Extension        = (*Extension)(nil)
	_ extension.StateInitializer = (*Extension)(nil)
	_ extension.Advertiser       = (*Extension)(nil)
	_ extension.Recovery         = (*Extension)(nil)
	_ extension.PlanSystemWriter = (*Extension)(nil)
	_ tool.ToolProvider          = (*Extension)(nil)
)

// Name implements [extension.Extension] and [tool.ToolProvider].
func (e *Extension) Name() string { return providerName }

// Lifetime implements [tool.ToolProvider]. Plan state lives in
// [extension.SessionState]; the provider itself is shared so
// PerAgent fits.
func (e *Extension) Lifetime() tool.Lifetime { return tool.LifetimePerAgent }

// agentParticipant returns the ParticipantInfo plan ext stamps on
// every emitted plan extension_frame. The plan tool path is always invoked
// by the agent (the model issues the tool call), so the author is
// the agent.
func (e *Extension) agentParticipant() protocol.ParticipantInfo {
	return protocol.ParticipantInfo{ID: e.agentID, Kind: protocol.ParticipantAgent}
}

// InitState implements [extension.StateInitializer]. Allocates a
// fresh [SessionPlan] handle for the calling session and stashes it
// under [StateKey]. The handle starts with an empty (Active=false)
// projection; Recovery on materialise replays plan extension_frame events into
// it.
func (e *Extension) InitState(_ context.Context, state extension.SessionState) error {
	state.SetValue(StateKey, &SessionPlan{
		sessionID: state.SessionID(),
		author:    e.agentParticipant(),
	})
	return nil
}

// FromState returns the [*SessionPlan] handle for state, or nil if
// the extension has not run InitState for it.
func FromState(state extension.SessionState) *SessionPlan {
	if state == nil {
		return nil
	}
	v, ok := state.Value(StateKey)
	if !ok {
		return nil
	}
	h, _ := v.(*SessionPlan)
	return h
}

// SessionPlan is the per-session typed handle the plan extension
// stores in [extension.SessionState]. Owns the in-memory [Plan]
// projection + a mutex serialising the emit-then-Apply sequence so
// concurrent tool handlers can't race the mirror; persisted events
// remain the source of truth so a desync (handler crashed mid-
// update, say) self-heals on the next materialise / restart.
type SessionPlan struct {
	sessionID string
	author    protocol.ParticipantInfo

	mu   sync.Mutex
	plan Plan
}

// Snapshot returns a copy of the in-memory projection. Tests use
// this to assert plan state without poking the handle internals.
func (h *SessionPlan) Snapshot() Plan {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.plan
}

// Render returns the system-prompt rendering of the projection or
// "" when no plan is active. Holding the mutex briefly ensures the
// read sees a consistent snapshot even if a tool handler is mid-
// Apply on another goroutine.
func (h *SessionPlan) Render(renderer *prompts.Renderer) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return Render(renderer, h.plan)
}

// AdvertiseSystemPrompt implements [extension.Advertiser]. Returns
// the rendered active-plan block, or "" when the plan is inactive.
func (e *Extension) AdvertiseSystemPrompt(_ context.Context, state extension.SessionState) string {
	h := FromState(state)
	if h == nil {
		return ""
	}
	return h.Render(state.Prompts())
}

// ---------- ToolProvider surface ----------

const (
	planSetSchema = `{
  "type": "object",
  "properties": {
    "text":         {"type": "string", "description": "Plan body. Capped at 8 KB after truncation marker."},
    "current_step": {"type": "string", "description": "Short pointer to the active step. Optional; preserves prior value when omitted."}
  },
  "required": ["text"]
}`

	planCommentSchema = `{
  "type": "object",
  "properties": {
    "text":         {"type": "string", "description": "Comment body. Capped at 2 KB after truncation marker."},
    "current_step": {"type": "string", "description": "Short pointer; optional, preserves prior value when omitted."}
  },
  "required": ["text"]
}`

	planShowSchema = `{
  "type": "object",
  "properties": {}
}`

	planClearSchema = `{
  "type": "object",
  "properties": {}
}`
)

// List implements [tool.ToolProvider].
func (e *Extension) List(_ context.Context) ([]tool.Tool, error) {
	return []tool.Tool{
		{
			Name:             providerName + ":set",
			Description:      "Write or replace the plan body. Wipes the in-memory comment log; events are not deleted.",
			Provider:         providerName,
			PermissionObject: PermObjectWrite,
			ArgSchema:        json.RawMessage(planSetSchema),
		},
		{
			Name:             providerName + ":comment",
			Description:      "Append a progress comment. Optionally moves the current-step pointer.",
			Provider:         providerName,
			PermissionObject: PermObjectWrite,
			ArgSchema:        json.RawMessage(planCommentSchema),
		},
		{
			Name:             providerName + ":show",
			Description:      "Return the full plan state — body + pointer + every retained comment since the last set.",
			Provider:         providerName,
			PermissionObject: PermObjectRead,
			ArgSchema:        json.RawMessage(planShowSchema),
		},
		{
			Name:             providerName + ":clear",
			Description:      "Drop the plan entirely. Body and pointer no longer render in the system prompt.",
			Provider:         providerName,
			PermissionObject: PermObjectWrite,
			ArgSchema:        json.RawMessage(planClearSchema),
		},
	}, nil
}

// Call implements [tool.ToolProvider]. Routes by short tool name
// after stripping the "plan:" prefix.
func (e *Extension) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	short := strings.TrimPrefix(name, providerName+":")
	switch short {
	case "set":
		return e.callSet(ctx, args)
	case "comment":
		return e.callComment(ctx, args)
	case "show":
		return e.callShow(ctx, args)
	case "clear":
		return e.callClear(ctx, args)
	default:
		return nil, fmt.Errorf("%w: plan:%s", tool.ErrUnknownTool, short)
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

type setInput struct {
	Text        string `json:"text"`
	CurrentStep string `json:"current_step,omitempty"`
}

type commentInput struct {
	Text        string `json:"text"`
	CurrentStep string `json:"current_step,omitempty"`
}

type okOutput struct {
	OK bool `json:"ok"`
}

type showOutput struct {
	Active      bool             `json:"active"`
	Text        string           `json:"text,omitempty"`
	CurrentStep string           `json:"current_step,omitempty"`
	SetAt       string           `json:"set_at,omitempty"`
	UpdatedAt   string           `json:"updated_at,omitempty"`
	Comments    []showCommentRow `json:"comments,omitempty"`
}

type showCommentRow struct {
	At          string `json:"at"`
	CurrentStep string `json:"current_step,omitempty"`
	Text        string `json:"text"`
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

// SystemSet writes the plan body on the calling session's state
// via the system principal — no ToolManager dispatch, no LLM
// round-trip, no permission gate. Used by the runtime to seed a
// mission's plan from its skill's metadata.hugen.mission.on_start.plan
// block before the mission's first model turn (phase 4.2.2 §7).
//
// Authorised callers: pkg/session/spawn.go only. Exposed as a
// method (not a top-level function) so the test infrastructure can
// still register an Extension instance and verify the write path
// through the standard ToolFilter / Advertiser pipeline.
func (e *Extension) SystemSet(ctx context.Context, state extension.SessionState, text, currentStep string) error {
	if state == nil {
		return errors.New("plan: SystemSet: state is nil")
	}
	h := FromState(state)
	if h == nil {
		return errors.New("plan: SystemSet: no plan handle on session state")
	}
	out, err := persistAndApply(ctx, state, h, OpSet, text, currentStep, false)
	if err != nil {
		return err
	}
	// persistAndApply may surface bad_request / io as a tool_error
	// envelope returning (bytes, nil). For the system path we lift
	// that into a real error so the runtime can fail-fast.
	if isToolErrorEnvelope(out) {
		return fmt.Errorf("plan: SystemSet: %s", out)
	}
	return nil
}

// isToolErrorEnvelope is a cheap discriminator: persistAndApply's
// success body is `{"ok":true}`; the envelope body always carries
// an "error" key.
func isToolErrorEnvelope(b []byte) bool {
	return len(b) > 0 && bytes.Contains(b, []byte(`"error":`))
}

func (e *Extension) callSet(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	state, h, errResp, err := stateAndHandle(ctx)
	if err != nil || errResp != nil {
		return errResp, err
	}
	var in setInput
	if err := json.Unmarshal(args, &in); err != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid plan:set args: %v", err))
	}
	if in.Text == "" {
		return toolErr("bad_request", "text is required")
	}
	return persistAndApply(ctx, state, h, OpSet, in.Text, in.CurrentStep, true)
}

func (e *Extension) callComment(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	state, h, errResp, err := stateAndHandle(ctx)
	if err != nil || errResp != nil {
		return errResp, err
	}
	var in commentInput
	if err := json.Unmarshal(args, &in); err != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid plan:comment args: %v", err))
	}
	if in.Text == "" {
		return toolErr("bad_request", "text is required")
	}
	h.mu.Lock()
	active := h.plan.Active
	h.mu.Unlock()
	if !active {
		return toolErr("no_active_plan",
			"no plan:set precedes this comment; call plan:set first")
	}
	return persistAndApply(ctx, state, h, OpComment, in.Text, in.CurrentStep, true)
}

func (e *Extension) callShow(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	_, h, errResp, err := stateAndHandle(ctx)
	if err != nil || errResp != nil {
		return errResp, err
	}
	h.mu.Lock()
	p := h.plan
	h.mu.Unlock()
	if !p.Active {
		return json.Marshal(showOutput{Active: false})
	}
	out := showOutput{
		Active:      true,
		Text:        p.Text,
		CurrentStep: p.CurrentStep,
		SetAt:       p.SetAt.UTC().Format(time.RFC3339),
		UpdatedAt:   p.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if len(p.Comments) > 0 {
		out.Comments = make([]showCommentRow, 0, len(p.Comments))
		for _, c := range p.Comments {
			out.Comments = append(out.Comments, showCommentRow{
				At:          c.At.UTC().Format(time.RFC3339),
				CurrentStep: c.CurrentStep,
				Text:        c.Text,
			})
		}
	}
	return json.Marshal(out)
}

func (e *Extension) callClear(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	state, h, errResp, err := stateAndHandle(ctx)
	if err != nil || errResp != nil {
		return errResp, err
	}
	return persistAndApply(ctx, state, h, OpClear, "", "", false)
}

// stateAndHandle resolves the calling session's state + plan handle
// from the dispatch ctx. Returns a JSON error response when the
// session or handle is missing rather than a Go error so the LLM
// sees a structured `{"error":...}` rather than an infrastructure
// failure.
func stateAndHandle(ctx context.Context) (extension.SessionState, *SessionPlan, json.RawMessage, error) {
	state, ok := extension.SessionStateFromContext(ctx)
	if !ok || state == nil {
		resp, err := toolErr("session_gone", "no session attached to dispatch ctx")
		return nil, nil, resp, err
	}
	h := FromState(state)
	if h == nil {
		resp, err := toolErr("unavailable", "plan extension state not initialised")
		return nil, nil, resp, err
	}
	return state, h, nil, nil
}

// persistAndApply is the shared write path for set / comment /
// clear: serialises emit-then-Apply under the handle's mutex,
// preserves the prior current_step pointer when the caller omits it
// (set / comment), and assigns the resulting projection back.
//
// preservePriorStep=true → set / comment use prior pointer when the
// caller omits current_step. Clear ignores the pointer entirely.
//
// Holding the mutex across emit serialises plan tool calls within a
// session — acceptable because plan ops are rare. If emit fails the
// in-memory mirror stays untouched, mirroring "events are the
// source of truth" — the next materialise will rebuild from
// whatever did land.
func persistAndApply(ctx context.Context, state extension.SessionState, h *SessionPlan, op, text, currentStep string, preservePriorStep bool) (json.RawMessage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if preservePriorStep && currentStep == "" {
		currentStep = h.plan.CurrentStep
	}

	data, err := json.Marshal(OpData{Text: text, CurrentStep: currentStep})
	if err != nil {
		return toolErr("io", fmt.Sprintf("marshal plan op data: %v", err))
	}
	frame := protocol.NewExtensionFrame(h.sessionID, h.author, providerName, protocol.CategoryOp, op, data)
	if err := state.Emit(ctx, frame); err != nil {
		return toolErr("io", fmt.Sprintf("emit plan op: %v", err))
	}
	h.plan = Apply(h.plan, ProjectEvent{
		At:          frame.OccurredAt(),
		Op:          op,
		Text:        text,
		CurrentStep: currentStep,
	})
	return json.Marshal(okOutput{OK: true})
}
