package session

import (
	"context"
	"encoding/json"
	"fmt"
)

// session:notepad_append delegates to the caller session's Notepad
// (per-session state). Handler signature receives *Session directly
// via SessionToolProvider — no ctx-stash recovery.

func init() {
	sessionTools["notepad_append"] = sessionToolDescriptor{
		Name:             "notepad_append",
		Description:      "Append a note to the caller's session notepad.",
		PermissionObject: permObjectNotepadAppend,
		ArgSchema:        json.RawMessage(notepadAppendSchema),
		Handler:          (*Session).callNotepadAppend,
	}
}

const permObjectNotepadAppend = "hugen:notepad:append"

const notepadAppendSchema = `{
  "type": "object",
  "properties": {
    "text": {"type": "string", "description": "Note body."},
    "author_id": {"type": "string", "description": "Optional author tag; defaults to the calling identity."}
  },
  "required": ["text"]
}`

type notepadAppendInput struct {
	Text     string `json:"text"`
	AuthorID string `json:"author_id,omitempty"`
}

func (s *Session) callNotepadAppend(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if s.IsClosed() {
		return toolErr("session_gone", "calling session has already terminated")
	}
	var in notepadAppendInput
	if err := json.Unmarshal(args, &in); err != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid notepad_append args: %v", err))
	}
	np := s.Notepad()
	if np == nil {
		return toolErr("unavailable", "session notepad not configured")
	}
	id, err := np.Append(ctx, in.AuthorID, in.Text)
	if err != nil {
		return toolErr("io", err.Error())
	}
	return json.Marshal(map[string]string{"id": id})
}
