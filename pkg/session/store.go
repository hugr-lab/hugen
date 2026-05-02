package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/store/queries"
)

// Sentinel errors returned by RuntimeStore implementations.
var (
	ErrSessionNotFound  = errors.New("runtime: session not found")
	ErrSessionDuplicate = errors.New("runtime: session already exists")
	ErrInvalidStatus    = errors.New("runtime: invalid session status")
	ErrSessionClosed    = errors.New("runtime: session is closed")
)

// Session lifecycle states. Stored on `sessions.status`.
const (
	StatusActive    = "active"
	StatusSuspended = "suspended"
	StatusClosed    = "closed"
)

// SessionRow mirrors the hub.db.agent.sessions row layout.
type SessionRow struct {
	ID                 string         `json:"id"`
	AgentID            string         `json:"agent_id"`
	OwnerID            string         `json:"owner_id,omitempty"`
	ParentSessionID    string         `json:"parent_session_id,omitempty"`
	SessionType        string         `json:"session_type"`
	SpawnedFromEventID string         `json:"spawned_from_event_id,omitempty"`
	Status             string         `json:"status"`
	Mission            string         `json:"mission,omitempty"`
	Metadata           map[string]any `json:"metadata,omitempty"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`
}

// EventRow mirrors hub.db.agent.session_events. Frame envelope fields
// are reconstructed from the columns on read; the variant payload is
// JSON-encoded into Content + ToolArgs + ToolResult + Metadata as
// dictated by the Frame kind.
type EventRow struct {
	ID         string         `json:"id"`
	SessionID  string         `json:"session_id"`
	AgentID    string         `json:"agent_id"`
	Seq        int            `json:"seq"`
	EventType  string         `json:"event_type"`
	Author     string         `json:"author"`
	Content    string         `json:"content,omitempty"`
	ToolName   string         `json:"tool_name,omitempty"`
	ToolArgs   map[string]any `json:"tool_args,omitempty"`
	ToolResult string         `json:"tool_result,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
}

// NoteRow mirrors hub.db.agent.session_notes.
type NoteRow struct {
	ID              string    `json:"id"`
	AgentID         string    `json:"agent_id"`
	SessionID       string    `json:"session_id"`
	AuthorSessionID string    `json:"author_session_id"`
	Content         string    `json:"content"`
	CreatedAt       time.Time `json:"created_at"`
}

// ListEventsOpts is the parameter bundle for RuntimeStore.ListEvents.
//
//   - MinSeq=0 returns events from the start of the session (phase-1
//     default; matches the previous int-only signature).
//   - MinSeq>0 returns events with seq strictly greater than MinSeq;
//     this is the reconnection-replay cursor consumed by
//     pkg/adapter/http (Last-Event-ID header). See R-Plan-20.
//   - Limit=0 means "use the implementation default" (1000 for the
//     local store).
type ListEventsOpts struct {
	MinSeq int
	Limit  int
}

// RuntimeStore is the persistence facade consumed by Session and
// Manager. Declared at the consumer per constitution principle
// III; *RuntimeStoreLocal below is the phase-1 implementation.
type RuntimeStore interface {
	OpenSession(ctx context.Context, row SessionRow) error
	LoadSession(ctx context.Context, id string) (SessionRow, error)
	UpdateSessionStatus(ctx context.Context, id, status string) error
	AppendEvent(ctx context.Context, ev EventRow, summary string) error
	ListEvents(ctx context.Context, sessionID string, opts ListEventsOpts) ([]EventRow, error)
	NextSeq(ctx context.Context, sessionID string) (int, error)
	AppendNote(ctx context.Context, note NoteRow) error
	ListNotes(ctx context.Context, sessionID string, limit int) ([]NoteRow, error)
	ListSessions(ctx context.Context, agentID, status string) ([]SessionRow, error)
}

// RuntimeStoreLocal is the DuckDB-backed implementation over
// pkg/store/local through types.Querier.
//
// embedderEnabled gates the "summary:" mutation argument: when the
// hugr engine has no embedder attached, the schema doesn't expose
// the argument and the server rejects mutations that pass it.
type RuntimeStoreLocal struct {
	querier         types.Querier
	embedderEnabled bool
}

// NewRuntimeStoreLocal constructs the local-store facade.
func NewRuntimeStoreLocal(q types.Querier, embedderEnabled bool) *RuntimeStoreLocal {
	return &RuntimeStoreLocal{querier: q, embedderEnabled: embedderEnabled}
}

func (s *RuntimeStoreLocal) OpenSession(ctx context.Context, row SessionRow) error {
	if row.ID == "" {
		return fmt.Errorf("runtime store: OpenSession requires ID")
	}
	if row.AgentID == "" {
		return fmt.Errorf("runtime store: OpenSession requires AgentID")
	}
	if row.SessionType == "" {
		row.SessionType = "root"
	}
	if row.Status == "" {
		row.Status = StatusActive
	}
	data := map[string]any{
		"id":           row.ID,
		"agent_id":     row.AgentID,
		"status":       row.Status,
		"session_type": row.SessionType,
	}
	if row.OwnerID != "" {
		data["owner_id"] = row.OwnerID
	}
	if row.Metadata != nil {
		data["metadata"] = row.Metadata
	}
	return queries.RunMutation(ctx, s.querier,
		`mutation ($data: hub_db_sessions_mut_input_data!) {
			hub { db { agent {
				insert_sessions(data: $data) { id }
			}}}
		}`,
		map[string]any{"data": data},
	)
}

func (s *RuntimeStoreLocal) LoadSession(ctx context.Context, id string) (SessionRow, error) {
	rows, err := queries.RunQuery[[]SessionRow](ctx, s.querier,
		`query ($id: String!) {
			hub { db { agent {
				sessions(filter: {id: {eq: $id}}, limit: 1) {
					id agent_id owner_id parent_session_id session_type spawned_from_event_id
					status mission metadata created_at updated_at
				}
			}}}
		}`,
		map[string]any{"id": id},
		"hub.db.agent.sessions",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return SessionRow{}, ErrSessionNotFound
		}
		return SessionRow{}, err
	}
	if len(rows) == 0 {
		return SessionRow{}, ErrSessionNotFound
	}
	return rows[0], nil
}

func (s *RuntimeStoreLocal) UpdateSessionStatus(ctx context.Context, id, status string) error {
	if status != StatusActive && status != StatusSuspended && status != StatusClosed {
		return fmt.Errorf("%w: %q", ErrInvalidStatus, status)
	}
	return queries.RunMutation(ctx, s.querier,
		`mutation ($id: String!, $data: hub_db_sessions_mut_data!) {
			hub { db { agent {
				update_sessions(filter: {id: {eq: $id}}, data: $data) { affected_rows }
			}}}
		}`,
		map[string]any{
			"id":   id,
			"data": map[string]any{"status": status},
		},
	)
}

func (s *RuntimeStoreLocal) NextSeq(ctx context.Context, sessionID string) (int, error) {
	type row struct {
		Seq int `json:"seq"`
	}
	rows, err := queries.RunQuery[[]row](ctx, s.querier,
		`query ($sid: String!) {
			hub { db { agent {
				session_events(
					filter: {session_id: {eq: $sid}},
					order_by: [{field: "seq", direction: DESC}],
					limit: 1
				) { seq }
			}}}
		}`,
		map[string]any{"sid": sessionID},
		"hub.db.agent.session_events",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return 1, nil
		}
		return 0, err
	}
	if len(rows) == 0 {
		return 1, nil
	}
	return rows[0].Seq + 1, nil
}

func (s *RuntimeStoreLocal) AppendEvent(ctx context.Context, ev EventRow, summary string) error {
	if ev.ID == "" {
		return fmt.Errorf("runtime store: AppendEvent requires ID")
	}
	if ev.SessionID == "" {
		return fmt.Errorf("runtime store: AppendEvent requires SessionID")
	}
	if ev.EventType == "" {
		return fmt.Errorf("runtime store: AppendEvent requires EventType")
	}
	if ev.AgentID == "" {
		return fmt.Errorf("runtime store: AppendEvent requires AgentID")
	}
	if ev.Seq == 0 {
		next, err := s.NextSeq(ctx, ev.SessionID)
		if err != nil {
			return fmt.Errorf("runtime store: nextSeq: %w", err)
		}
		ev.Seq = next
	}
	if ev.Author == "" {
		ev.Author = ev.AgentID
	}
	data := map[string]any{
		"id":         ev.ID,
		"session_id": ev.SessionID,
		"agent_id":   ev.AgentID,
		"seq":        ev.Seq,
		"event_type": ev.EventType,
		"author":     ev.Author,
	}
	if ev.Content != "" {
		data["content"] = ev.Content
	}
	if ev.ToolName != "" {
		data["tool_name"] = ev.ToolName
	}
	if ev.ToolArgs != nil {
		data["tool_args"] = ev.ToolArgs
	}
	if ev.ToolResult != "" {
		data["tool_result"] = ev.ToolResult
	}
	if ev.Metadata != nil {
		data["metadata"] = ev.Metadata
	}
	if summary == "" || !s.embedderEnabled {
		return queries.RunMutation(ctx, s.querier,
			`mutation ($data: hub_db_session_events_mut_input_data!) {
				hub { db { agent {
					insert_session_events(data: $data) { id }
				}}}
			}`,
			map[string]any{"data": data},
		)
	}
	return queries.RunMutation(ctx, s.querier,
		`mutation ($data: hub_db_session_events_mut_input_data!, $summary: String) {
			hub { db { agent {
				insert_session_events(data: $data, summary: $summary) { id }
			}}}
		}`,
		map[string]any{"data": data, "summary": summary},
	)
}

func (s *RuntimeStoreLocal) ListEvents(ctx context.Context, sessionID string, opts ListEventsOpts) ([]EventRow, error) {
	if opts.Limit <= 0 {
		opts.Limit = 1000
	}
	filter := map[string]any{"session_id": map[string]any{"eq": sessionID}}
	if opts.MinSeq > 0 {
		filter["seq"] = map[string]any{"gt": opts.MinSeq}
	}
	rows, err := queries.RunQuery[[]EventRow](ctx, s.querier,
		`query ($filter: hub_db_session_events_filter, $limit: Int) {
			hub { db { agent {
				session_events(
					filter: $filter,
					order_by: [{field: "seq", direction: ASC}],
					limit: $limit
				) {
					id session_id agent_id seq event_type author content
					tool_name tool_args tool_result metadata created_at
				}
			}}}
		}`,
		map[string]any{"filter": filter, "limit": opts.Limit},
		"hub.db.agent.session_events",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	return rows, nil
}

func (s *RuntimeStoreLocal) AppendNote(ctx context.Context, note NoteRow) error {
	if note.ID == "" {
		return fmt.Errorf("runtime store: AppendNote requires ID")
	}
	if note.SessionID == "" {
		return fmt.Errorf("runtime store: AppendNote requires SessionID")
	}
	if note.Content == "" {
		return fmt.Errorf("runtime store: AppendNote requires Content")
	}
	if note.AuthorSessionID == "" {
		note.AuthorSessionID = note.SessionID
	}
	data := map[string]any{
		"id":                note.ID,
		"agent_id":          note.AgentID,
		"session_id":        note.SessionID,
		"author_session_id": note.AuthorSessionID,
		"content":           note.Content,
	}
	return queries.RunMutation(ctx, s.querier,
		`mutation ($data: hub_db_session_notes_mut_input_data!) {
			hub { db { agent {
				insert_session_notes(data: $data) { id }
			}}}
		}`,
		map[string]any{"data": data},
	)
}

func (s *RuntimeStoreLocal) ListNotes(ctx context.Context, sessionID string, limit int) ([]NoteRow, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := queries.RunQuery[[]NoteRow](ctx, s.querier,
		`query ($sid: String!, $limit: Int) {
			hub { db { agent {
				session_notes(
					filter: {session_id: {eq: $sid}},
					order_by: [{field: "created_at", direction: ASC}],
					limit: $limit
				) {
					id agent_id session_id author_session_id content created_at
				}
			}}}
		}`,
		map[string]any{"sid": sessionID, "limit": limit},
		"hub.db.agent.session_notes",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	return rows, nil
}

func (s *RuntimeStoreLocal) ListSessions(ctx context.Context, agentID, status string) ([]SessionRow, error) {
	filter := map[string]any{"agent_id": map[string]any{"eq": agentID}}
	if status != "" {
		filter["status"] = map[string]any{"eq": status}
	}
	rows, err := queries.RunQuery[[]SessionRow](ctx, s.querier,
		`query ($filter: hub_db_sessions_filter) {
			hub { db { agent {
				sessions(filter: $filter, order_by: [{field: "updated_at", direction: DESC}]) {
					id agent_id owner_id parent_session_id session_type spawned_from_event_id
					status mission metadata created_at updated_at
				}
			}}}
		}`,
		map[string]any{"filter": filter},
		"hub.db.agent.sessions",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	return rows, nil
}

// FrameToEventRow projects a Frame onto the columnar EventRow shape
// for persistence. Mapping rules:
//
//   - Content carries the human-readable text for chat-like kinds
//     (user/agent/reasoning/error/system_marker subject).
//   - ToolName + ToolArgs + ToolResult carry tool_call / tool_result.
//   - Metadata carries the full JSON payload so all variant fields
//     round-trip on read (chunk_seq/final/details/...).
func FrameToEventRow(f protocol.Frame, agentID string) (EventRow, string, error) {
	codec := protocol.NewCodec()
	payloadBytes, err := codec.EncodePayload(f)
	if err != nil {
		return EventRow{}, "", err
	}
	var meta map[string]any
	if len(payloadBytes) > 0 {
		if err := json.Unmarshal(payloadBytes, &meta); err != nil {
			meta = nil
		}
	}
	row := EventRow{
		ID:        f.FrameID(),
		SessionID: f.SessionID(),
		AgentID:   agentID,
		EventType: string(f.Kind()),
		Author:    f.Author().ID,
		Metadata:  meta,
		CreatedAt: f.OccurredAt(),
	}
	var summary string
	switch v := f.(type) {
	case *protocol.UserMessage:
		row.Content = v.Payload.Text
		summary = v.Payload.Text
	case *protocol.AgentMessage:
		row.Content = v.Payload.Text
		if v.Payload.Final {
			summary = v.Payload.Text
		}
	case *protocol.Reasoning:
		row.Content = v.Payload.Text
	case *protocol.SlashCommand:
		row.Content = v.Payload.Raw
	case *protocol.Cancel:
		row.Content = v.Payload.Reason
	case *protocol.SessionClosed:
		row.Content = v.Payload.Reason
	case *protocol.Error:
		row.Content = v.Payload.Message
	case *protocol.SystemMarker:
		row.Content = v.Payload.Subject
	case *protocol.ToolCall:
		row.ToolName = v.Payload.Name
		if args, ok := v.Payload.Args.(map[string]any); ok {
			row.ToolArgs = args
		}
	case *protocol.ToolResult:
		if str, ok := v.Payload.Result.(string); ok {
			row.ToolResult = str
		} else if v.Payload.Result != nil {
			b, _ := json.Marshal(v.Payload.Result)
			row.ToolResult = string(b)
		}
	}
	return row, summary, nil
}

// EventRowToFrame is the inverse of FrameToEventRow. It uses the
// Metadata column (full JSON payload) to reconstruct the variant
// payload precisely, falling back to columnar fields when Metadata
// is absent (older rows / minimal callers).
func EventRowToFrame(row EventRow) (protocol.Frame, error) {
	base := protocol.BaseFrame{
		ID:      row.ID,
		Session: row.SessionID,
		K:       protocol.Kind(row.EventType),
		Auth: protocol.ParticipantInfo{
			ID:   row.Author,
			Kind: deriveAuthorKind(row.Author, row.AgentID),
		},
		At: row.CreatedAt,
		S:  row.Seq,
	}
	codec := protocol.NewCodec()
	var payload []byte
	if row.Metadata != nil {
		b, err := json.Marshal(row.Metadata)
		if err == nil {
			payload = b
		}
	}
	if len(payload) == 0 {
		// Synthesise a minimal payload from the columnar fields.
		switch base.K {
		case protocol.KindUserMessage, protocol.KindAgentMessage, protocol.KindReasoning:
			payload = []byte(fmt.Sprintf(`{"text":%q,"chunk_seq":0,"final":true}`, row.Content))
		case protocol.KindError:
			payload = []byte(fmt.Sprintf(`{"code":"unknown","message":%q}`, row.Content))
		case protocol.KindSystemMarker:
			payload = []byte(fmt.Sprintf(`{"subject":%q}`, row.Content))
		case protocol.KindSessionClosed, protocol.KindCancel:
			payload = []byte(fmt.Sprintf(`{"reason":%q}`, row.Content))
		default:
			payload = []byte(`{}`)
		}
	}
	return codec.DecodePayload(base, payload)
}

func deriveAuthorKind(author, agentID string) string {
	switch {
	case author == "" || author == "system" || author == "hugen":
		return protocol.ParticipantSystem
	case author == agentID || author == "agent" || author == "tool":
		return protocol.ParticipantAgent
	default:
		return protocol.ParticipantUser
	}
}
