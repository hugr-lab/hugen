// Package notepad bundles everything for the notepad session
// extension into one place: the per-session [Notepad] type
// (Append / Read / Search / Show around the persistence layer),
// the narrow [Store] interface RuntimeStore satisfies, and the
// [Extension] wrapper that exposes the four tools as a
// tool.ToolProvider and stashes per-session [Notepad] handles in
// extension.SessionState under [StateKey].
//
// The extension is an agent-level singleton constructed once at
// runtime boot with a Store + the agent's id + a [Config]; every
// session gets its own *Notepad handle initialised by InitState.
// Phase 4.2.3 — InitState snapshots the writing session's
// rootID via the parent-chain walk, then every Append / Read /
// Search uses that root id as the storage key so missions
// spawned later in the same root conversation see the same
// notepad.
package notepad

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// StateKey is the [extension.SessionState] key the extension
// stores its per-session [Notepad] handle under.
const StateKey = "notepad"

// Permission objects gated by Tier-1 operator config + Tier-2
// Hugr role rules + Tier-3 per-user policies. Per-tier narrowing
// is the skill manifest's job (allowed-tools); the extension
// exposes the full surface and lets the perm stack decide.
const (
	PermAppend = "hugen:notepad:append"
	PermRead   = "hugen:notepad:read"
	PermSearch = "hugen:notepad:search"
	PermShow   = "hugen:notepad:show"
)

const providerName = "notepad"

// Config carries operator-tunable knobs.
type Config struct {
	// Window caps how far back read/search/snapshot reach. Zero
	// falls back to DefaultWindow (48h). The append-only
	// constitution means notes older than the window stay in the
	// table but fall out of model visibility.
	Window time.Duration
}

// Extension implements [extension.Extension] +
// [extension.StateInitializer] + [tool.ToolProvider].
type Extension struct {
	store   Store
	agentID string
	cfg     Config
}

// NewExtension constructs the notepad extension. cfg.Window <=0
// resolves to DefaultWindow inside per-session Notepads.
func NewExtension(s Store, agentID string, cfg Config) *Extension {
	return &Extension{store: s, agentID: agentID, cfg: cfg}
}

// Compile-time interface assertions.
var (
	_ extension.Extension        = (*Extension)(nil)
	_ extension.StateInitializer = (*Extension)(nil)
	_ extension.Instructor       = (*Extension)(nil) // phase 5.2 π — was Advertiser
	_ tool.ToolProvider          = (*Extension)(nil)
)

func (e *Extension) Name() string            { return providerName }
func (e *Extension) Lifetime() tool.Lifetime { return tool.LifetimePerAgent }

// PerTurnPrompt implements [extension.Instructor] — Block B per
// phase 4.2.3 §5. Returns a compact snapshot of recent notes
// (within the configured window), grouped by category, ordered by
// most-recent-write. Capped to keep the prompt budget tight; the
// model is expected to call notepad:search / notepad:read for full
// content when relevant. Empty string when the notepad is empty or
// the state handle is missing.
//
// Phase 5.2 π — migrated from Advertiser. The notepad's content
// changes turn-to-turn (every notepad:write between turns) so it
// belongs in the per-turn dynamic inject (end of prompt, outside
// the cached static prefix), not in the always-rebuilt-but-
// expected-to-be-stable static system prompt.
func (e *Extension) PerTurnPrompt(ctx context.Context, state extension.SessionState) string {
	np := FromState(state)
	if np == nil {
		return ""
	}
	notes, err := np.Read(ctx, ReadInput{Limit: maxReadLimit})
	if err != nil || len(notes) == 0 {
		return ""
	}
	return renderSnapshot(state.Prompts(), notes, np.Window())
}

// ReportStatus implements [extension.StatusReporter]. Returns a
// compact projection: the 10 most-recent notes plus the TRUE
// per-category totals (server-side bucket aggregation, not derived
// from the truncated recent list). Phase 5.1c — the TUI sidebar
// renders the totals; recent list is kept for future "recent notes"
// expansion panels. Nil when no notes or no notepad on this session.
//
// Wire shape:
//
//	{"recent": [{"id":..., "category":..., ...}, ...], "counts": {"cat": N, ...}}
func (e *Extension) ReportStatus(ctx context.Context, state extension.SessionState) json.RawMessage {
	np := FromState(state)
	if np == nil {
		return nil
	}
	counts, err := np.CountsByCategory(ctx)
	if err != nil {
		return nil
	}
	notes, err := np.Read(ctx, ReadInput{Limit: 10})
	if err != nil {
		return nil
	}
	if len(notes) == 0 && len(counts) == 0 {
		return nil
	}
	payload := struct {
		Recent []wireNote     `json:"recent"`
		Counts map[string]int `json:"counts"`
	}{
		Recent: notesToWire(notes),
		Counts: counts,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return data
}

// InitState allocates a fresh [Notepad] for the calling session.
// rootID is resolved once via the parent-chain walk and the
// per-Notepad role label is derived from depth. Both are stable
// for the session's lifetime so caching them here is safe.
func (e *Extension) InitState(_ context.Context, state extension.SessionState) error {
	rootID := WalkToRootID(state)
	role := skill.TierFromDepth(state.Depth())
	state.SetValue(StateKey, New(e.store, e.agentID, state.SessionID(), rootID, role, e.cfg.Window))
	return nil
}

// FromState returns the *Notepad handle for state, or nil if the
// extension's InitState hasn't run for it (e.g. a session
// created without the notepad extension registered).
func FromState(state extension.SessionState) *Notepad {
	v, ok := state.Value(StateKey)
	if !ok {
		return nil
	}
	n, _ := v.(*Notepad)
	return n
}

// ---------- ToolProvider surface ----------

const appendSchema = `{
  "type": "object",
  "properties": {
    "content":  {"type": "string", "description": "Concise hypothesis or finding — treat as observation under uncertainty, not a validated fact. Phrase with hedging when unsure (\"appears to\", \"in the sample we checked\")."},
    "category": {"type": "string", "description": "Open-string filtering tag (e.g. \"schema-finding\", \"user-preference\", \"deferred-question\"). Optional but recommended for retrieval."},
    "mission":  {"type": "string", "description": "Short phrase describing what this session is working on right now. Optional — empty for ad-hoc notes."}
  },
  "required": ["content"]
}`

const readSchema = `{
  "type": "object",
  "properties": {
    "category": {"type": "string", "description": "Restrict to notes carrying this category tag. Optional."},
    "limit":    {"type": "integer", "minimum": 1, "maximum": 50, "description": "Max rows; default 20."}
  }
}`

const searchSchema = `{
  "type": "object",
  "properties": {
    "query":    {"type": "string", "description": "Natural-language semantic query. Required."},
    "category": {"type": "string", "description": "Restrict to notes carrying this category tag. Optional."},
    "limit":    {"type": "integer", "minimum": 1, "maximum": 20, "description": "Max rows; default 5."}
  },
  "required": ["query"]
}`

const showSchema = `{
  "type": "object",
  "properties": {
    "category": {"type": "string", "description": "Restrict to notes carrying this category tag. Optional."},
    "limit":    {"type": "integer", "minimum": 1, "maximum": 50, "description": "Max rows; default 20."}
  }
}`

// List implements [tool.ToolProvider].
func (e *Extension) List(_ context.Context) ([]tool.Tool, error) {
	return []tool.Tool{
		{
			Name:             providerName + ":append",
			Description:      "Append a working note (hypothesis) to the conversation's session-scoped notepad. Visible to every mission / worker spawned in the same root session via notepad:read / notepad:search.",
			Provider:         providerName,
			PermissionObject: PermAppend,
			ArgSchema:        json.RawMessage(appendSchema),
		},
		{
			Name:             providerName + ":read",
			Description:      "List recent notes from the conversation's notepad (within the configured read window). Returns hypotheses, not validated facts.",
			Provider:         providerName,
			PermissionObject: PermRead,
			ArgSchema:        json.RawMessage(readSchema),
		},
		{
			Name:             providerName + ":search",
			Description:      "Semantic search over the conversation's notepad. Falls back to recency ordering when no embedder is attached.",
			Provider:         providerName,
			PermissionObject: PermSearch,
			ArgSchema:        json.RawMessage(searchSchema),
		},
		{
			Name:             providerName + ":show",
			Description:      "Render the notepad as human-readable Markdown for the user (root-tier; the caller is expected to relay the result verbatim).",
			Provider:         providerName,
			PermissionObject: PermShow,
			ArgSchema:        json.RawMessage(showSchema),
		},
	}, nil
}

// Call implements [tool.ToolProvider].
func (e *Extension) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	short := stripProviderPrefix(name)
	switch short {
	case "append":
		return e.callAppend(ctx, args)
	case "read":
		return e.callRead(ctx, args)
	case "search":
		return e.callSearch(ctx, args)
	case "show":
		return e.callShow(ctx, args)
	default:
		return nil, fmt.Errorf("%w: notepad:%s", tool.ErrUnknownTool, short)
	}
}

// Subscribe / Close — stateless surface; nothing to advertise or
// release at the provider level (per-session state has its own
// lifecycle through extension.Closer if needed in the future).
func (e *Extension) Subscribe(_ context.Context) (<-chan tool.ProviderEvent, error) {
	return nil, nil
}
func (e *Extension) Close() error { return nil }

func stripProviderPrefix(name string) string {
	pfx := providerName + ":"
	if len(name) > len(pfx) && name[:len(pfx)] == pfx {
		return name[len(pfx):]
	}
	return name
}

// ---------- tool-dispatch handlers ----------

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

// notepadFromCtx resolves the calling session's Notepad handle.
// Centralises the SessionState + InitState check so individual
// handlers stay small.
func notepadFromCtx(ctx context.Context) (*Notepad, json.RawMessage, error) {
	state, ok := extension.SessionStateFromContext(ctx)
	if !ok {
		out, err := toolErr("session_gone", "no session attached to dispatch ctx")
		return nil, out, err
	}
	np := FromState(state)
	if np == nil {
		out, err := toolErr("unavailable", "notepad extension state not initialised")
		return nil, out, err
	}
	return np, nil, nil
}

func (e *Extension) callAppend(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	np, errOut, err := notepadFromCtx(ctx)
	if np == nil {
		return errOut, err
	}
	var in AppendInput
	if uerr := json.Unmarshal(args, &in); uerr != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid notepad:append args: %v", uerr))
	}
	id, aerr := np.Append(ctx, in)
	if aerr != nil {
		return toolErr("io", aerr.Error())
	}
	return json.Marshal(map[string]string{"id": id})
}

func (e *Extension) callRead(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	np, errOut, err := notepadFromCtx(ctx)
	if np == nil {
		return errOut, err
	}
	var in ReadInput
	if len(args) > 0 {
		if uerr := json.Unmarshal(args, &in); uerr != nil {
			return toolErr("bad_request", fmt.Sprintf("invalid notepad:read args: %v", uerr))
		}
	}
	notes, rerr := np.Read(ctx, in)
	if rerr != nil {
		return toolErr("io", rerr.Error())
	}
	return json.Marshal(map[string]any{"notes": notesToWire(notes)})
}

func (e *Extension) callSearch(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	np, errOut, err := notepadFromCtx(ctx)
	if np == nil {
		return errOut, err
	}
	var in SearchInput
	if uerr := json.Unmarshal(args, &in); uerr != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid notepad:search args: %v", uerr))
	}
	notes, serr := np.Search(ctx, in)
	if serr != nil {
		return toolErr("io", serr.Error())
	}
	return json.Marshal(map[string]any{"notes": notesToWire(notes)})
}

func (e *Extension) callShow(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	np, errOut, err := notepadFromCtx(ctx)
	if np == nil {
		return errOut, err
	}
	var in ShowInput
	if len(args) > 0 {
		if uerr := json.Unmarshal(args, &in); uerr != nil {
			return toolErr("bad_request", fmt.Sprintf("invalid notepad:show args: %v", uerr))
		}
	}
	out, serr := np.Show(ctx, in)
	if serr != nil {
		return toolErr("io", serr.Error())
	}
	return json.Marshal(map[string]string{"text": out})
}

// wireNote is the JSON shape returned to the model. Trimmed of
// internal fields (storage SessionID is irrelevant — it's always
// the root) to keep the tool envelope tight.
type wireNote struct {
	ID         string `json:"id"`
	Category   string `json:"category,omitempty"`
	AuthorRole string `json:"author_role,omitempty"`
	Mission    string `json:"mission,omitempty"`
	Text       string `json:"text"`
	CreatedAt  string `json:"created_at"`
}

func notesToWire(notes []Note) []wireNote {
	out := make([]wireNote, 0, len(notes))
	for _, n := range notes {
		out = append(out, wireNote{
			ID:         n.ID,
			Category:   n.Category,
			AuthorRole: n.AuthorRole,
			Mission:    n.Mission,
			Text:       n.Text,
			CreatedAt:  n.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out
}
