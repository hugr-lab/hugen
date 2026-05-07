// Package notepad is the session extension that exposes the
// per-session notepad as the LLM-facing tool "notepad:append" and
// stashes the per-session [notepad.Notepad] handle in
// [extension.SessionState] under the key "notepad".
//
// The extension is an agent-level singleton constructed once at
// runtime boot with a notepad.Store + the agent's id; every
// session gets its own *notepad.Notepad handle initialised by
// InitState.
package notepad

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/extension"
	notepadpkg "github.com/hugr-lab/hugen/pkg/session/tools/notepad"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// StateKey is the [extension.SessionState] key the extension stores its
// per-session [notepad.Notepad] handle under. Exported so callers
// looking up the handle from outside the extension (legacy
// session.Notepad accessor, command env wiring) can do so without
// magic strings.
const StateKey = "notepad"

// PermObject is the permission object the runtime gates the
// notepad:append tool on. Mirrored verbatim from the legacy
// session: tool entry so existing config keeps working.
const PermObject = "hugen:notepad:append"

// providerName is the catalogue prefix the LLM sees:
// "notepad:<tool>". Matches tool.ToolProvider semantics.
const providerName = "notepad"

// Extension implements [extension.Extension] +
// [extension.StateInitializer] + [tool.ToolProvider]. The instance
// is shared across every session under one Manager; per-session
// state lives in [extension.SessionState] under [StateKey].
type Extension struct {
	store   notepadpkg.Store
	agentID string
}

// New constructs the notepad extension. store is the persistence
// surface (typically the agent's RuntimeStore) and agentID is the
// owning agent's id stamped onto every NoteRow.
func New(store notepadpkg.Store, agentID string) *Extension {
	return &Extension{store: store, agentID: agentID}
}

// Compile-time interface assertions.
var (
	_ extension.Extension        = (*Extension)(nil)
	_ extension.StateInitializer = (*Extension)(nil)
	_ tool.ToolProvider          = (*Extension)(nil)
)

// Name implements [extension.Extension] and [tool.ToolProvider].
// Doubles as the catalogue prefix and the [StateKey].
func (e *Extension) Name() string { return providerName }

// Lifetime implements [tool.ToolProvider]. The provider is
// stateless (per-session state lives in [extension.SessionState]) so
// PerAgent fits — one provider instance shared across sessions.
func (e *Extension) Lifetime() tool.Lifetime { return tool.LifetimePerAgent }

// InitState implements [extension.StateInitializer]. Allocates a
// fresh [notepad.Notepad] for the calling session and stashes it
// under [StateKey].
func (e *Extension) InitState(_ context.Context, state extension.SessionState) error {
	state.SetValue(StateKey, notepadpkg.New(e.store, e.agentID, state.SessionID()))
	return nil
}

// FromState returns the *notepad.Notepad handle for state, or nil
// if the extension has not run InitState for it (e.g. a session
// created without the notepad extension registered).
func FromState(state extension.SessionState) *notepadpkg.Notepad {
	v, ok := state.Value(StateKey)
	if !ok {
		return nil
	}
	n, _ := v.(*notepadpkg.Notepad)
	return n
}

// ---------- ToolProvider surface ----------

const appendSchema = `{
  "type": "object",
  "properties": {
    "text": {"type": "string", "description": "Note body."},
    "author_id": {"type": "string", "description": "Optional author tag; defaults to the calling identity."}
  },
  "required": ["text"]
}`

// List implements [tool.ToolProvider].
func (e *Extension) List(_ context.Context) ([]tool.Tool, error) {
	return []tool.Tool{{
		Name:             providerName + ":append",
		Description:      "Append a note to the caller's session notepad.",
		Provider:         providerName,
		PermissionObject: PermObject,
		ArgSchema:        json.RawMessage(appendSchema),
	}}, nil
}

// Call implements [tool.ToolProvider]. Routes by short tool name
// after stripping the "notepad:" prefix.
func (e *Extension) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	short := name
	if pfx := providerName + ":"; len(name) > len(pfx) && name[:len(pfx)] == pfx {
		short = name[len(pfx):]
	}
	switch short {
	case "append":
		return e.callAppend(ctx, args)
	default:
		return nil, fmt.Errorf("%w: notepad:%s", tool.ErrUnknownTool, short)
	}
}

// Subscribe implements [tool.ToolProvider]. The catalogue is
// static.
func (e *Extension) Subscribe(_ context.Context) (<-chan tool.ProviderEvent, error) {
	return nil, nil
}

// Close implements [tool.ToolProvider]. The provider holds no
// resources of its own — per-session state cleanup happens via
// [extension.Closer] on the matching [extension.SessionState].
// Notepad's state is plain memory + store-mediated rows, so no
// Close hook is needed.
func (e *Extension) Close() error { return nil }

// ---------- handlers ----------

type appendInput struct {
	Text     string `json:"text"`
	AuthorID string `json:"author_id,omitempty"`
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

func (e *Extension) callAppend(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	state, ok := extension.SessionStateFromContext(ctx)
	if !ok {
		return toolErr("session_gone", "no session attached to dispatch ctx")
	}
	np := FromState(state)
	if np == nil {
		return toolErr("unavailable", "notepad extension state not initialised")
	}
	var in appendInput
	if err := json.Unmarshal(args, &in); err != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid notepad:append args: %v", err))
	}
	id, err := np.Append(ctx, in.AuthorID, in.Text)
	if err != nil {
		return toolErr("io", err.Error())
	}
	return json.Marshal(map[string]string{"id": id})
}
