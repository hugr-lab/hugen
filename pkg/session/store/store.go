package store

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

// NoteRow mirrors hub.db.agent.session_notes — the persistence
// shape RuntimeStore implementations work with. JSON tags match
// queries.RunQuery decoding. The notepad extension owns the
// in-memory Note type and the higher-level wrapper around this
// shape; the row itself stays here next to RuntimeStore.AppendNote
// so the persistence column stays the single source of truth.
//
// Phase 4.2.3 added Category / AuthorRole / Mission. The embedding
// Vector column on the table is intentionally absent from this
// struct — Hugr populates it server-side via the `summary:`
// mutation argument and the runtime never reads the raw vector
// back into Go.
type NoteRow struct {
	ID              string    `json:"id"`
	AgentID         string    `json:"agent_id"`
	SessionID       string    `json:"session_id"`
	AuthorSessionID string    `json:"author_session_id"`
	Category        string    `json:"category,omitempty"`
	AuthorRole      string    `json:"author_role,omitempty"`
	Mission         string    `json:"mission,omitempty"`
	Content         string    `json:"content"`
	CreatedAt       time.Time `json:"created_at"`
}

// ListNotesOpts narrows the result set returned by ListNotes /
// SearchNotes. Phase 4.2.3 — short-term-memory framing relies on
// `Window` for natural eviction (no archival, no DELETE).
//
//   - Window: maximum age of returned notes; zero = no time cutoff.
//   - Category: open-string filter; empty = all categories.
//   - Limit: max rows; <=0 = implementation default (50 for
//     ListNotes, 5 for SearchNotes).
type ListNotesOpts struct {
	Window   time.Duration
	Category string
	Limit    int
}

// Sentinel errors returned by RuntimeStore implementations.
var (
	ErrSessionNotFound  = errors.New("runtime: session not found")
	ErrSessionDuplicate = errors.New("runtime: session already exists")
	ErrInvalidStatus    = errors.New("runtime: invalid session status")
	ErrSessionClosed    = errors.New("runtime: session is closed")
	// ErrNoEmbedder is returned by SearchNotes when the runtime
	// store has no embedder data source attached — semantic search
	// is impossible. Callers typically fall back to ListNotes
	// ordered by recency.
	ErrNoEmbedder = errors.New("runtime: no embedder attached")
)

// Session lifecycle states. Stored on `sessions.status`.
//
// `sessions.status` is authoritative for liveness queries. The live
// Session writes its own column update from teardown
// ([Session.handleExit]) right after appending the
// `session_terminated` event — single owner, single write. The event
// log stays append-only (constitution §"Append-only persistence on
// memory tables") and remains the durable record; the column is a
// queryable cache on top of it that lets list paths filter by an
// indexed scalar instead of probing event rows.
//
// Crash between event-append and column-update leaves the row
// "stuck" on Active. Resume picker treats stuck rows as resumable —
// next live close (`/end`) flips the column. Reconciliation passes
// (e.g. a future cron) can sweep them too.
//
// Legacy "suspended" / "closed" values are dropped — phase-4 never
// wrote them and no reader special-cases them.
const (
	StatusActive     = "active"
	StatusTerminated = "terminated"
)

// EventTypeRoutingOp is reserved for phase-5 HITL chain forwarding.
// Phase-4 declares the constant so the routing layer can reference it
// but no producer emits it yet.
const EventTypeRoutingOp = "routing_op"

// EventTypeHumanMessageReceived / EventTypeAssistantMessageSent give
// `parent_context` (US1) explicit categorisation rows to filter on,
// independent of the streaming chunk markers `KindUserMessage` /
// `KindAgentMessage`. Producers wire these in commit 5 (run-loop
// refactor); this commit only declares the constants so consumers
// can reference them.
const (
	EventTypeHumanMessageReceived = "human_message_received"
	EventTypeAssistantMessageSent = "assistant_message_sent"
)

// ResumableRoot is one root session returned by
// [RuntimeStoreLocal.ListResumableRoots]: the row plus its most
// recent lifecycle event (latest [protocol.KindSessionStatus]) fetched
// in the same nested GraphQL query so the resume classifier doesn't
// have to make a second round-trip per session. `Lifecycle` is at
// most one row (limit=1, order_by created_at DESC) and may be empty
// for sessions that never wrote a session_status frame.
type ResumableRoot struct {
	SessionRow
	Lifecycle []EventRow `json:"events,omitempty"`
}

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

// ListEventsOpts is the parameter bundle for RuntimeStore.ListEvents.
//
//   - MinSeq=0 returns events from the start of the session (phase-1
//     default; matches the previous int-only signature).
//   - MinSeq>0 returns events with seq strictly greater than MinSeq;
//     this is the reconnection-replay cursor consumed by
//     pkg/adapter/http (Last-Event-ID header). See R-Plan-20.
//   - Limit=0 means "use the implementation default" (1000 for the
//     local store).
//   - Kinds, when non-empty, narrows the query to those event_type
//     values via `event_type: { in: kinds }`. Empty = all kinds.
//     Use this to push event-type filters down to the database
//     instead of materialising the entire log only to discard most
//     of it (e.g. settle's subagent_result scan,
//     drainCachedSubagentResults, roleAndTaskForNudge).
//   - MetadataContains, when non-nil, narrows further via Hugr's
//     JSON `contains` operator (PostgreSQL `@>` semantics) — match
//     rows whose metadata column is a superset of the given map.
//     Combine with Kinds for tight scans like
//     "subagent_started where child_session_id = X".
//   - From / To, when non-zero, bound `created_at` (inclusive). Use
//     for time-window scans (parent_context).
//   - SemanticQuery, when non-empty AND the store has an embedder
//     attached, ranks the result by similarity to that text via
//     Hugr's `semantic: { query, limit }` argument (the
//     [`@embeddings` directive](docs/8-references/1-directives.md)
//     on `session_events`). Ordering switches from seq ASC to
//     similarity DESC. Without an embedder the search is silently
//     dropped — caller falls back to time-ordered Kinds filter.
type ListEventsOpts struct {
	MinSeq           int
	Limit            int
	Kinds            []string
	MetadataContains map[string]any
	From             time.Time
	To               time.Time
	SemanticQuery    string
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
	// LatestEventOfKinds returns the newest EventRow whose
	// EventType matches one of kinds. ok=false (with nil err) when
	// no such row exists; non-nil err only on backing-store I/O
	// failure. Used by Manager.RestoreActive to classify a session
	// without loading its full event log.
	LatestEventOfKinds(ctx context.Context, sessionID string, kinds []string) (EventRow, bool, error)
	NextSeq(ctx context.Context, sessionID string) (int, error)
	// AppendNote inserts a note row. When the underlying store has
	// an embedder attached, note.Content is also passed as the
	// `summary:` mutation argument so Hugr generates and stores
	// the vector embedding atomically.
	AppendNote(ctx context.Context, note NoteRow) error
	// ListNotes returns notes for sessionID filtered by opts. Order:
	// created_at DESC (newest first). opts.Window applies a
	// `created_at >= NOW() - Window` cutoff; opts.Category narrows
	// further when non-empty; opts.Limit caps the row count.
	ListNotes(ctx context.Context, sessionID string, opts ListNotesOpts) ([]NoteRow, error)
	// SearchNotes runs a semantic search for notes belonging to
	// sessionID via Hugr's `semantic: {query, limit}` top-level
	// argument. The query string is embedded server-side under the
	// @embeddings directive on session_notes. Without an embedder
	// attached the method returns ErrNoEmbedder so the caller can
	// fall back to ListNotes. Same Window/Category/Limit semantics
	// as ListNotes; ordering is by similarity DESC.
	SearchNotes(ctx context.Context, sessionID, query string, opts ListNotesOpts) ([]NoteRow, error)
	// CountNotesByCategory returns one row per distinct category
	// with the total note count. opts.Window applies the same
	// `created_at >= NOW() - Window` cutoff as ListNotes; opts.Category
	// is honoured only as a redundant pre-filter (the result map will
	// contain at most that one key); opts.Limit is ignored — bucket
	// rows are bounded by distinct category count, not by note count.
	// Notes with an empty category are bucketed under the empty key.
	CountNotesByCategory(ctx context.Context, sessionID string, opts ListNotesOpts) (map[string]int, error)
	ListSessions(ctx context.Context, agentID, status string) ([]SessionRow, error)
	// ListResumableRoots returns every root session for agentID
	// whose `status` column is Active. Each row carries its most
	// recent [protocol.KindSessionStatus] event nested under
	// `Lifecycle` so the caller (Manager.RestoreActive) can classify
	// idle / active / wait_* without a second round-trip per row.
	// Ordered by `updated_at DESC` so callers that want "the
	// freshest resumable" can take rows[0]. The nested event sub-
	// query is bounded `limit=1` and `order_by created_at DESC`.
	ListResumableRoots(ctx context.Context, agentID string) ([]ResumableRoot, error)
	// ListChildren returns every session whose parent_session_id equals
	// parentID. Used by the phase-4 restart BFS walker to traverse
	// parent→child trees on boot. Returns an empty slice (not an error)
	// when parentID has no children.
	ListChildren(ctx context.Context, parentID string) ([]SessionRow, error)
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
	// Pre-existing bug fix (caught phase 4.2.2 δ.A): the parent
	// linkage fields must reach the insert mutation, otherwise
	// every subagent row lands with parent_session_id=NULL and
	// sessions(filter: parent_session_id: eq) finds no children
	// even though the spawn tree is correct in memory.
	if row.ParentSessionID != "" {
		data["parent_session_id"] = row.ParentSessionID
	}
	if row.SpawnedFromEventID != "" {
		data["spawned_from_event_id"] = row.SpawnedFromEventID
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
	if status != StatusActive && status != StatusTerminated {
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
	if len(opts.Kinds) > 0 {
		filter["event_type"] = map[string]any{"in": opts.Kinds}
	}
	if len(opts.MetadataContains) > 0 {
		filter["metadata"] = map[string]any{"contains": opts.MetadataContains}
	}
	if !opts.From.IsZero() || !opts.To.IsZero() {
		ts := map[string]any{}
		if !opts.From.IsZero() {
			ts["gte"] = opts.From.UTC().Format(time.RFC3339Nano)
		}
		if !opts.To.IsZero() {
			ts["lte"] = opts.To.UTC().Format(time.RFC3339Nano)
		}
		filter["created_at"] = ts
	}
	// Semantic ranking: when the store has an embedder attached and
	// the caller passed a non-empty query, switch to Hugr's
	// `semantic: { query, limit }` argument. Hugr generates the query
	// embedding internally (per the @embeddings directive on
	// session_events) and orders by similarity. Filters still apply
	// first (Hugr docs §"Filter Combined with Vector Search":
	// 1) filter, 2) similarity, 3) limit).
	if opts.SemanticQuery != "" && s.embedderEnabled {
		rows, err := queries.RunQuery[[]EventRow](ctx, s.querier,
			`query ($filter: hub_db_session_events_filter, $semantic: SemanticSearchInput) {
				hub { db { agent {
					session_events(filter: $filter, semantic: $semantic) {
						id session_id agent_id seq event_type author content
						tool_name tool_args tool_result metadata created_at
					}
				}}}
			}`,
			map[string]any{
				"filter":   filter,
				"semantic": map[string]any{"query": opts.SemanticQuery, "limit": opts.Limit},
			},
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

// LatestEventOfKinds runs a narrow query (DESC by seq, limit 1,
// event_type IN kinds) so RestoreActive can classify a session
// without paging the full event log.
func (s *RuntimeStoreLocal) LatestEventOfKinds(ctx context.Context, sessionID string, kinds []string) (EventRow, bool, error) {
	if len(kinds) == 0 {
		return EventRow{}, false, nil
	}
	filter := map[string]any{
		"session_id": map[string]any{"eq": sessionID},
		"event_type": map[string]any{"in": kinds},
	}
	rows, err := queries.RunQuery[[]EventRow](ctx, s.querier,
		`query ($filter: hub_db_session_events_filter) {
			hub { db { agent {
				session_events(
					filter: $filter,
					order_by: [{field: "seq", direction: DESC}],
					limit: 1
				) {
					id session_id agent_id seq event_type author content
					tool_name tool_args tool_result metadata created_at
				}
			}}}
		}`,
		map[string]any{"filter": filter},
		"hub.db.agent.session_events",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return EventRow{}, false, nil
		}
		return EventRow{}, false, err
	}
	if len(rows) == 0 {
		return EventRow{}, false, nil
	}
	return rows[0], true, nil
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
	if note.Category != "" {
		data["category"] = note.Category
	}
	if note.AuthorRole != "" {
		data["author_role"] = note.AuthorRole
	}
	if note.Mission != "" {
		data["mission"] = note.Mission
	}
	if !s.embedderEnabled {
		return queries.RunMutation(ctx, s.querier,
			`mutation ($data: hub_db_session_notes_mut_input_data!) {
				hub { db { agent {
					insert_session_notes(data: $data) { id }
				}}}
			}`,
			map[string]any{"data": data},
		)
	}
	// summary: triggers server-side embedding generation per the
	// @embeddings directive on session_notes. The note's content
	// is short and self-contained — use it verbatim as the summary.
	return queries.RunMutation(ctx, s.querier,
		`mutation ($data: hub_db_session_notes_mut_input_data!, $summary: String) {
			hub { db { agent {
				insert_session_notes(data: $data, summary: $summary) { id }
			}}}
		}`,
		map[string]any{"data": data, "summary": note.Content},
	)
}

// notesProjection is the column set ListNotes and SearchNotes
// project. Kept as a constant so the two paths stay in sync.
const notesProjection = `
	id agent_id session_id author_session_id
	category author_role mission
	content created_at
`

func (s *RuntimeStoreLocal) ListNotes(ctx context.Context, sessionID string, opts ListNotesOpts) ([]NoteRow, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	filter := buildNotesFilter(sessionID, opts)
	rows, err := queries.RunQuery[[]NoteRow](ctx, s.querier,
		`query ($filter: hub_db_session_notes_filter, $limit: Int) {
			hub { db { agent {
				session_notes(
					filter: $filter,
					order_by: [{field: "created_at", direction: DESC}],
					limit: $limit
				) {`+notesProjection+`}
			}}}
		}`,
		map[string]any{"filter": filter, "limit": limit},
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

func (s *RuntimeStoreLocal) SearchNotes(ctx context.Context, sessionID, query string, opts ListNotesOpts) ([]NoteRow, error) {
	if !s.embedderEnabled {
		return nil, ErrNoEmbedder
	}
	if query == "" {
		return nil, fmt.Errorf("runtime store: SearchNotes requires non-empty query")
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 5
	}
	filter := buildNotesFilter(sessionID, opts)
	rows, err := queries.RunQuery[[]NoteRow](ctx, s.querier,
		`query ($filter: hub_db_session_notes_filter, $semantic: SemanticSearchInput) {
			hub { db { agent {
				session_notes(filter: $filter, semantic: $semantic) {`+notesProjection+`}
			}}}
		}`,
		map[string]any{
			"filter":   filter,
			"semantic": map[string]any{"query": query, "limit": limit},
		},
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

// CountNotesByCategory groups notes by `category` server-side via
// Hugr's `session_notes_bucket_aggregation` and returns the total
// per category. Reuses buildNotesFilter so the cutoff/category
// semantics match ListNotes exactly. Phase 5.1c — feeds the TUI
// liveview sidebar's total-per-tag breakdown without round-tripping
// the full notes table over the GraphQL wire.
func (s *RuntimeStoreLocal) CountNotesByCategory(ctx context.Context, sessionID string, opts ListNotesOpts) (map[string]int, error) {
	filter := buildNotesFilter(sessionID, opts)
	type bucketRow struct {
		Key struct {
			Category string `json:"category"`
		} `json:"key"`
		Aggregations struct {
			RowsCount int `json:"_rows_count"`
		} `json:"aggregations"`
	}
	rows, err := queries.RunQuery[[]bucketRow](ctx, s.querier,
		`query ($filter: hub_db_session_notes_filter) {
			hub { db { agent {
				session_notes_bucket_aggregation(filter: $filter) {
					key { category }
					aggregations { _rows_count }
				}
			}}}
		}`,
		map[string]any{"filter": filter},
		"hub.db.agent.session_notes_bucket_aggregation",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	out := make(map[string]int, len(rows))
	for _, r := range rows {
		if r.Aggregations.RowsCount == 0 {
			continue
		}
		out[r.Key.Category] = r.Aggregations.RowsCount
	}
	return out, nil
}

// buildNotesFilter constructs the hub_db_session_notes_filter map
// shared by ListNotes and SearchNotes. Phase 4.2.3 — sessionID is
// always the storage location (root after climb-to-root); the
// optional Window applies a created_at cutoff and Category narrows
// by tag.
func buildNotesFilter(sessionID string, opts ListNotesOpts) map[string]any {
	filter := map[string]any{"session_id": map[string]any{"eq": sessionID}}
	if opts.Window > 0 {
		cutoff := time.Now().UTC().Add(-opts.Window).Format(time.RFC3339Nano)
		filter["created_at"] = map[string]any{"gte": cutoff}
	}
	if opts.Category != "" {
		filter["category"] = map[string]any{"eq": opts.Category}
	}
	return filter
}

func (s *RuntimeStoreLocal) ListChildren(ctx context.Context, parentID string) ([]SessionRow, error) {
	if parentID == "" {
		return nil, fmt.Errorf("runtime store: ListChildren requires parent id")
	}
	filter := map[string]any{"parent_session_id": map[string]any{"eq": parentID}}
	rows, err := queries.RunQuery[[]SessionRow](ctx, s.querier,
		`query ($filter: hub_db_sessions_filter) {
			hub { db { agent {
				sessions(filter: $filter, order_by: [{field: "created_at", direction: ASC}]) {
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

func (s *RuntimeStoreLocal) ListResumableRoots(ctx context.Context, agentID string) ([]ResumableRoot, error) {
	filter := map[string]any{
		"agent_id":     map[string]any{"eq": agentID},
		"session_type": map[string]any{"eq": "root"},
		"status":       map[string]any{"eq": StatusActive},
	}
	eventFilter := map[string]any{
		"event_type": map[string]any{"eq": string(protocol.KindSessionStatus)},
	}
	rows, err := queries.RunQuery[[]ResumableRoot](ctx, s.querier,
		`query ($filter: hub_db_sessions_filter, $events_filter: hub_db_session_events_filter) {
			hub { db { agent {
				sessions(filter: $filter, order_by: [{field: "updated_at", direction: DESC}]) {
					id agent_id owner_id parent_session_id session_type spawned_from_event_id
					status mission metadata created_at updated_at
					events(filter: $events_filter, order_by: [{field: "created_at", direction: DESC}], limit: 1) {
						id session_id agent_id seq event_type author content metadata created_at
					}
				}
			}}}
		}`,
		map[string]any{
			"filter":        filter,
			"events_filter": eventFilter,
		},
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
	case *protocol.SubagentStarted:
		row.Content = v.Payload.Task
	case *protocol.SubagentResult:
		row.Content = v.Payload.Result
	case *protocol.ExtensionFrame:
		// Extension-owned events (whiteboard, skill, notepad, plan, …)
		// put their human-readable surface — when one exists — into
		// Content so query / digest paths that read Content directly
		// still see it. Today whiteboard write + plan set/comment ops
		// carry a `text` field in their JSON data payload; skill /
		// notepad use other fields. The codec already round-trips the
		// full payload through Metadata, so a missing Content here is
		// harmless.
		if len(v.Payload.Data) > 0 {
			var data struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(v.Payload.Data, &data); err == nil {
				row.Content = data.Text
			}
		}
	case *protocol.SessionTerminated:
		row.Content = v.Payload.Reason
	case *protocol.SystemMessage:
		row.Content = v.Payload.Content
	}
	// Phase-4 envelope additions: persist the cross-session sender id
	// in Metadata so EventRowToFrame can re-hydrate it precisely. The
	// other two reserved fields (FromParticipant, RequestID) ride
	// the same Metadata map when set.
	if from := f.FromSessionID(); from != "" && row.Metadata != nil {
		row.Metadata["__from_session"] = from
	}
	if from := f.FromParticipantID(); from != "" && row.Metadata != nil {
		row.Metadata["__from_participant"] = from
	}
	if req := f.RequestIDValue(); req != "" && row.Metadata != nil {
		row.Metadata["__request_id"] = req
	}
	return row, summary, nil
}

// EventRowToFrame is the inverse of FrameToEventRow. It uses the
// Metadata column (full JSON payload) to reconstruct the variant
// payload precisely, falling back to columnar fields when Metadata
// is absent (older rows / minimal callers).
//
// Phase-4 envelope additions (FromSession, FromParticipant,
// RequestID) ride the Metadata map under reserved keys (__from_session,
// __from_participant, __request_id). The codec ignores unknown keys
// when unmarshalling into the typed payload, so the overlay does not
// need to be stripped from the payload bytes.
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
	if row.Metadata != nil {
		if v, ok := row.Metadata["__from_session"].(string); ok {
			base.FromSession = v
		}
		if v, ok := row.Metadata["__from_participant"].(string); ok {
			base.FromParticipant = v
		}
		if v, ok := row.Metadata["__request_id"].(string); ok {
			base.RequestID = v
		}
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
		case protocol.KindSessionTerminated:
			payload = []byte(fmt.Sprintf(`{"reason":%q}`, row.Content))
		case protocol.KindSystemMessage:
			payload = []byte(fmt.Sprintf(`{"kind":"unknown","content":%q}`, row.Content))
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
