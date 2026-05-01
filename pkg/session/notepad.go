package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

const noteMaxBytes = 64 * 1024

// Note is the in-memory representation of a session note.
type Note struct {
	ID        string
	SessionID string
	AuthorID  string
	Text      string
	CreatedAt time.Time
}

// Notepad gives a Session a typed handle on its notes table. All
// writes go through RuntimeStore.AppendNote.
type Notepad struct {
	store     RuntimeStore
	agentID   string
	sessionID string
}

// NewNotepad constructs a Notepad bound to one session.
func NewNotepad(store RuntimeStore, agentID, sessionID string) *Notepad {
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
	row := NoteRow{
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
