package notepad

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/session/store"
	"github.com/hugr-lab/hugen/pkg/skill"
)

const (
	// noteMaxBytes caps the content of a single note. Hypotheses
	// are expected to be short; this bound also protects the
	// prompt budget when notes round-trip into Block B (γ).
	noteMaxBytes = 64 * 1024

	// DefaultWindow is the read/search/snapshot cutoff used when
	// Config.Window is unset. Notes older than this fall out of
	// model visibility but remain in the table — the append-only
	// constitution forbids deletion sweeps. 48h is the spec
	// footer's recommended starting point.
	DefaultWindow = 48 * time.Hour

	defaultReadLimit   = 20
	defaultSearchLimit = 5
	maxReadLimit       = 50
	maxSearchLimit     = 20
)

// Note is the in-memory representation of a session note.
type Note struct {
	ID         string
	SessionID  string // storage location (root)
	AuthorID   string // authoring session id
	AuthorRole string // tier label: root | mission | worker
	Category   string
	Mission    string
	Text       string
	CreatedAt  time.Time
}

// AppendInput is the tool input for notepad:append. Content is
// required; the rest is optional metadata the model populates so
// future missions can retrieve and weight hypotheses correctly.
type AppendInput struct {
	Content  string `json:"content"`
	Category string `json:"category,omitempty"`
	Mission  string `json:"mission,omitempty"`
}

// ReadInput / SearchInput / ShowInput share a filtering vocabulary
// — only SearchInput additionally requires Query.
type ReadInput struct {
	Category string `json:"category,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

type SearchInput struct {
	Query    string `json:"query"`
	Category string `json:"category,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

type ShowInput struct {
	Category string `json:"category,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

// Store is the narrow persistence surface the notepad needs.
// session/store.RuntimeStore satisfies it implicitly.
type Store interface {
	AppendNote(ctx context.Context, row store.NoteRow) error
	ListNotes(ctx context.Context, sessionID string, opts store.ListNotesOpts) ([]store.NoteRow, error)
	SearchNotes(ctx context.Context, sessionID, query string, opts store.ListNotesOpts) ([]store.NoteRow, error)
}

// Notepad gives a Session a typed handle on its notes table. Phase
// 4.2.3 — all writes climb to rootID (snapshotted at InitState
// time from the parent chain). Reads use rootID directly; the
// session_notes_chain view is retained in the schema for
// observability but not consumed by the runtime hot path.
type Notepad struct {
	store     Store
	agentID   string
	sessionID string        // writing session
	rootID    string        // storage target (== sessionID for root sessions)
	role      string        // tier label, set by skill.TierFromDepth
	window    time.Duration // read/search cutoff
}

// New constructs a Notepad bound to one session. rootID empty
// defaults to sessionID (root-of-itself); role empty defaults to
// skill.TierRoot; window <=0 defaults to DefaultWindow.
func New(s Store, agentID, sessionID, rootID, role string, window time.Duration) *Notepad {
	if rootID == "" {
		rootID = sessionID
	}
	if role == "" {
		role = skill.TierRoot
	}
	if window <= 0 {
		window = DefaultWindow
	}
	return &Notepad{
		store:     s,
		agentID:   agentID,
		sessionID: sessionID,
		rootID:    rootID,
		role:      role,
		window:    window,
	}
}

// Append stores a note under the conversation's root session.
func (n *Notepad) Append(ctx context.Context, in AppendInput) (string, error) {
	content := strings.TrimSpace(in.Content)
	if content == "" {
		return "", fmt.Errorf("notepad: empty content")
	}
	if len(content) > noteMaxBytes {
		return "", fmt.Errorf("notepad: content exceeds %d bytes", noteMaxBytes)
	}
	id := newNoteID()
	row := store.NoteRow{
		ID:              id,
		AgentID:         n.agentID,
		SessionID:       n.rootID,
		AuthorSessionID: n.sessionID,
		Category:        strings.TrimSpace(in.Category),
		AuthorRole:      n.role,
		Mission:         strings.TrimSpace(in.Mission),
		Content:         content,
		CreatedAt:       time.Now().UTC(),
	}
	if err := n.store.AppendNote(ctx, row); err != nil {
		return "", fmt.Errorf("notepad: append: %w", err)
	}
	return id, nil
}

// Read returns recent notes within the window, DESC by created_at.
func (n *Notepad) Read(ctx context.Context, in ReadInput) ([]Note, error) {
	limit := clampLimit(in.Limit, defaultReadLimit, maxReadLimit)
	rows, err := n.store.ListNotes(ctx, n.rootID, store.ListNotesOpts{
		Window:   n.window,
		Category: strings.TrimSpace(in.Category),
		Limit:    limit,
	})
	if err != nil {
		return nil, err
	}
	return rowsToNotes(rows), nil
}

// Search runs a semantic search via Hugr's `semantic:` arg. When
// no embedder is attached, gracefully degrades to a recency listing
// so callers still get something usable. Phase 7's distillation
// pipeline can revisit older notes; the live model is fine with
// "less ranked" results here.
func (n *Notepad) Search(ctx context.Context, in SearchInput) ([]Note, error) {
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return nil, fmt.Errorf("notepad: empty query")
	}
	limit := clampLimit(in.Limit, defaultSearchLimit, maxSearchLimit)
	opts := store.ListNotesOpts{
		Window:   n.window,
		Category: strings.TrimSpace(in.Category),
		Limit:    limit,
	}
	rows, err := n.store.SearchNotes(ctx, n.rootID, query, opts)
	if err != nil {
		if errors.Is(err, store.ErrNoEmbedder) {
			rows, err = n.store.ListNotes(ctx, n.rootID, opts)
			if err != nil {
				return nil, err
			}
			return rowsToNotes(rows), nil
		}
		return nil, err
	}
	return rowsToNotes(rows), nil
}

// Show formats notes for the user (not for the model). Use case:
// "what have you noticed?" — root calls Show and renders the
// returned string into the assistant message. Tier-3 permission
// machinery is the authority on whether the caller may invoke it;
// the extension itself serves the data unconditionally so the
// surface stays predictable from tests.
func (n *Notepad) Show(ctx context.Context, in ShowInput) (string, error) {
	notes, err := n.Read(ctx, ReadInput{Category: in.Category, Limit: in.Limit})
	if err != nil {
		return "", err
	}
	return formatForUser(notes, n.window), nil
}

// Window returns the configured cutoff. Exposed for Block B
// rendering and tests.
func (n *Notepad) Window() time.Duration { return n.window }

// RootID returns the storage session id (root of the parent
// chain). Exposed for Block B rendering and tests.
func (n *Notepad) RootID() string { return n.rootID }

// Role returns the tier label assigned at construction time.
// Exposed for tests; the production write path uses it
// internally as author_role.
func (n *Notepad) Role() string { return n.role }

// ----------------------------------------------------------------
// helpers
// ----------------------------------------------------------------

func clampLimit(requested, def, max int) int {
	if requested <= 0 {
		return def
	}
	if requested > max {
		return max
	}
	return requested
}

func rowsToNotes(rows []store.NoteRow) []Note {
	out := make([]Note, 0, len(rows))
	for _, r := range rows {
		out = append(out, Note{
			ID:         r.ID,
			SessionID:  r.SessionID,
			AuthorID:   r.AuthorSessionID,
			AuthorRole: r.AuthorRole,
			Category:   r.Category,
			Mission:    r.Mission,
			Text:       r.Content,
			CreatedAt:  r.CreatedAt,
		})
	}
	return out
}

// formatForUser renders notes as a human-readable Markdown block.
// Notes are grouped by category; groups are ordered by their most
// recent note's CreatedAt (DESC). Each note line carries an age
// label and the author tier so the user can see at a glance which
// sub-agent contributed what.
func formatForUser(notes []Note, window time.Duration) string {
	if len(notes) == 0 {
		return fmt.Sprintf("Notepad is empty within the last %s.", windowLabel(window))
	}
	type group struct {
		category string
		notes    []Note
	}
	var groups []group
	idx := map[string]int{}
	for _, n := range notes {
		cat := n.Category
		if cat == "" {
			cat = "(uncategorised)"
		}
		i, ok := idx[cat]
		if !ok {
			groups = append(groups, group{category: cat})
			i = len(groups) - 1
			idx[cat] = i
		}
		groups[i].notes = append(groups[i].notes, n)
	}
	sort.SliceStable(groups, func(i, j int) bool {
		return groups[i].notes[0].CreatedAt.After(groups[j].notes[0].CreatedAt)
	})
	var b strings.Builder
	fmt.Fprintf(&b, "Notepad — last %s\n\n", windowLabel(window))
	for _, g := range groups {
		fmt.Fprintf(&b, "## %s (%d)\n", g.category, len(g.notes))
		for _, n := range g.notes {
			fmt.Fprintf(&b, "  - %s — %s ago, %s\n",
				oneLineSnippet(n.Text, 80),
				ageLabel(time.Since(n.CreatedAt)),
				n.AuthorRole,
			)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func oneLineSnippet(s string, max int) string {
	flat := strings.Join(strings.Fields(s), " ")
	if len(flat) <= max {
		return flat
	}
	return flat[:max-1] + "…"
}

// ageLabel renders a Duration as the coarsest readable unit.
// Approximate by design — Block B uses the same shape and weak
// models cope better with rounded values.
func ageLabel(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}

func windowLabel(d time.Duration) string {
	switch {
	case d <= 0:
		return "all time"
	case d >= 24*time.Hour && d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	default:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
}

func newNoteID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("note-%d", time.Now().UnixNano())
	}
	return "note-" + hex.EncodeToString(b[:])
}

// WalkToRootID returns the root session's id for the given state
// by walking Parent() until the chain ends. The parent chain
// doesn't mutate during a session's lifetime, so the extension's
// InitState calls this once and snapshots the result.
func WalkToRootID(state extension.SessionState) string {
	cur := state
	for {
		parent, ok := cur.Parent()
		if !ok {
			return cur.SessionID()
		}
		cur = parent
	}
}
