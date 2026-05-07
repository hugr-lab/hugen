package notepad

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/hugr-lab/hugen/pkg/session/store"
)

const noteMaxBytes = 64 * 1024

// Note is the in-memory representation of a session note returned
// by [Notepad.List]. The persistence shape lives next to the
// store interface as [store.NoteRow]; Note is the ergonomic view
// extensions and slash-command handlers consume.
type Note struct {
	ID        string
	SessionID string
	AuthorID  string
	Text      string
	CreatedAt time.Time
}

// Store is the narrow persistence surface the notepad needs. The
// session/store.RuntimeStore satisfies it implicitly.
type Store interface {
	AppendNote(ctx context.Context, row store.NoteRow) error
	ListNotes(ctx context.Context, sessionID string, limit int) ([]store.NoteRow, error)
}

// Notepad gives a Session a typed handle on its notes table. All
// writes go through Store.AppendNote.
type Notepad struct {
	store     Store
	agentID   string
	sessionID string
}

// New constructs a Notepad bound to one session.
func New(store Store, agentID, sessionID string) *Notepad {
	return &Notepad{store: store, agentID: agentID, sessionID: sessionID}
}

// Append stores a note. Returns the assigned note id.
func (n *Notepad) Append(ctx context.Context, authorID, text string) (string, error) {
	if text == "" {
		return "", fmt.Errorf("notepad: empty text")
	}
	if len(text) > noteMaxBytes {
		return "", fmt.Errorf("notepad: text exceeds %d bytes", noteMaxBytes)
	}
	id := newNoteID()
	row := store.NoteRow{
		ID:              id,
		AgentID:         n.agentID,
		SessionID:       n.sessionID,
		AuthorSessionID: n.sessionID,
		Content:         text,
		CreatedAt:       time.Now().UTC(),
	}
	if authorID == "" {
		authorID = n.agentID
	}
	_ = authorID // author_id is not in NoteRow; reserved for later spec
	if err := n.store.AppendNote(ctx, row); err != nil {
		return "", fmt.Errorf("notepad: append: %w", err)
	}
	return id, nil
}

// List returns up to limit notes ordered by created_at ASC.
func (n *Notepad) List(ctx context.Context, limit int) ([]Note, error) {
	rows, err := n.store.ListNotes(ctx, n.sessionID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Note, 0, len(rows))
	for _, r := range rows {
		out = append(out, Note{
			ID:        r.ID,
			SessionID: r.SessionID,
			AuthorID:  r.AuthorSessionID,
			Text:      r.Content,
			CreatedAt: r.CreatedAt,
		})
	}
	return out, nil
}

func newNoteID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("note-%d", time.Now().UnixNano())
	}
	return "note-" + hex.EncodeToString(b[:])
}
