package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
    "query": {"type": "string", "description": "Optional case-insensitive substring filter."},
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

	var fromT, toT time.Time
	var hasFrom, hasTo bool
	if in.From != "" {
		t, err := time.Parse(time.RFC3339, in.From)
		if err != nil {
			return toolErr("bad_request", fmt.Sprintf("from must be RFC3339: %v", err))
		}
		fromT = t
		hasFrom = true
	}
	if in.To != "" {
		t, err := time.Parse(time.RFC3339, in.To)
		if err != nil {
			return toolErr("bad_request", fmt.Sprintf("to must be RFC3339: %v", err))
		}
		toT = t
		hasTo = true
	}
	q := strings.ToLower(strings.TrimSpace(in.Query))

	rows, err := caller.store.ListEvents(ctx, parentID, store.ListEventsOpts{Limit: 1000})
	if err != nil {
		return toolErr("io", err.Error())
	}

	out := parentContextOutput{Messages: []parentContextMessage{}}
	// Newest first per contract — walk in reverse and stop at the cap.
	for i := len(rows) - 1; i >= 0 && len(out.Messages) < limit; i-- {
		r := rows[i]
		role, ok := parentContextRole(r)
		if !ok {
			continue
		}
		if hasFrom && r.CreatedAt.Before(fromT) {
			continue
		}
		if hasTo && r.CreatedAt.After(toT) {
			continue
		}
		// AgentMessage rows are written per chunk; only the final
		// chunk is meaningful in a transcript window. Fall back to
		// "any non-empty content row" when metadata is absent.
		if r.EventType == string(protocol.KindAgentMessage) {
			final, _ := r.Metadata["final"].(bool)
			if !final {
				continue
			}
		}
		if q != "" && !strings.Contains(strings.ToLower(r.Content), q) {
			continue
		}
		out.Messages = append(out.Messages, parentContextMessage{
			Seq:     r.Seq,
			At:      r.CreatedAt,
			Role:    role,
			Content: r.Content,
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
