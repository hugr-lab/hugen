package session

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// parent_context — sub-agent's read-only window into its direct
// parent's user-facing transcript. Filtered to user / assistant
// message rows; tool calls, reasoning, internal events stay
// invisible to the child by design.

const parentContextSchema = `{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "Optional semantic search query — matches against parent transcript by meaning, not literal substring. Requires the embedder data source to be attached; falls back to chronological listing otherwise."},
    "from":  {"type": "string", "description": "Optional RFC3339 lower bound."},
    "to":    {"type": "string", "description": "Optional RFC3339 upper bound."},
    "limit": {"type": "integer", "minimum": 1, "maximum": 20}
  }
}`

// parentContextHardCap caps the per-call message slice. Matches
// the JSON-Schema upper bound; both layers enforce it.
const parentContextHardCap = 20

type parentContextInput struct {
	Query string `json:"query,omitempty"`
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

type parentContextMessage struct {
	Seq     int       `json:"seq"`
	At      time.Time `json:"at"`
	Role    string    `json:"role"`
	Content string    `json:"content"`
}

type parentContextOutput struct {
	Messages []parentContextMessage `json:"messages"`
}

func (caller *Session) callParentContext(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if caller.IsClosed() {
		return toolErr("session_gone", "calling session has already terminated")
	}
	if caller.parent == nil {
		return toolErr("no_parent", "calling session is a root session")
	}
	parentID := caller.parent.id

	var in parentContextInput
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return toolErr("bad_request", fmt.Sprintf("invalid parent_context args: %v", err))
		}
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 5
	}
	if limit > parentContextHardCap {
		limit = parentContextHardCap
	}

	opts := store.ListEventsOpts{
		Kinds: []string{
			string(protocol.KindUserMessage),
			string(protocol.KindAgentMessage),
			store.EventTypeHumanMessageReceived,
			store.EventTypeAssistantMessageSent,
		},
		Limit: limit,
	}
	if in.From != "" {
		t, err := time.Parse(time.RFC3339, in.From)
		if err != nil {
			return toolErr("bad_request", fmt.Sprintf("from must be RFC3339: %v", err))
		}
		opts.From = t
	}
	if in.To != "" {
		t, err := time.Parse(time.RFC3339, in.To)
		if err != nil {
			return toolErr("bad_request", fmt.Sprintf("to must be RFC3339: %v", err))
		}
		opts.To = t
	}
	if in.Query != "" {
		opts.SemanticQuery = in.Query
	}
	rows, err := caller.store.ListEvents(ctx, parentID, opts)
	if err != nil {
		return toolErr("io", err.Error())
	}
	// AgentMessage chunks are not persisted (Consolidated=false stays
	// outbox-only — see session.emit), so the only KindAgentMessage rows
	// here are the per-iteration consolidated records carrying full
	// turn text. Filter belt-and-braces in case a legacy DB still has
	// chunk rows.
	out := parentContextOutput{Messages: make([]parentContextMessage, 0, len(rows))}
	for _, r := range rows {
		role, ok := parentContextRole(r)
		if !ok {
			continue
		}
		if r.EventType == string(protocol.KindAgentMessage) {
			cons, _ := r.Metadata["consolidated"].(bool)
			if !cons {
				continue
			}
		}
		out.Messages = append(out.Messages, parentContextMessage{
			Seq:     r.Seq,
			At:      r.CreatedAt,
			Role:    role,
			Content: r.Content,
		})
	}
	// Caller contract is "newest first". Semantic ranking comes back
	// in similarity order; the chronological path comes back seq ASC.
	// Re-order only when there's no semantic ranking to preserve.
	if opts.SemanticQuery == "" {
		sort.SliceStable(out.Messages, func(i, j int) bool {
			return out.Messages[i].Seq > out.Messages[j].Seq
		})
	}
	return json.Marshal(out)
}

// parentContextRole maps an EventRow to the user/assistant role the
// parent_context surface exposes. Rows that don't represent
// user-facing communication are filtered upstream.
func parentContextRole(r store.EventRow) (string, bool) {
	switch r.EventType {
	case string(protocol.KindUserMessage), store.EventTypeHumanMessageReceived:
		return "user", true
	case string(protocol.KindAgentMessage), store.EventTypeAssistantMessageSent:
		return "assistant", true
	}
	return "", false
}
