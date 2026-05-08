package session

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hugr-lab/hugen/pkg/session/store"
)

// subagent_runs — paginated transcript pull-through for a sub-agent
// the calling session spawned. Reads the child's events store
// (cross-session read gated by assertChildOf so a session can't
// peek at sub-agents it didn't spawn).

const subagentRunsSchema = `{
  "type": "object",
  "properties": {
    "session_id": {"type": "string"},
    "since_seq":  {"type": "integer", "minimum": 0},
    "limit":      {"type": "integer", "minimum": 1, "maximum": 500}
  },
  "required": ["session_id"]
}`

// subagentRunsHardCap caps the per-call event slice. Matches the
// JSON-Schema upper bound; the schema enforces it client-side, the
// const enforces it server-side too.
const subagentRunsHardCap = 500

type subagentRunsInput struct {
	SessionID string `json:"session_id"`
	SinceSeq  int    `json:"since_seq,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

type subagentRunsEvent struct {
	Seq       int             `json:"seq"`
	At        time.Time       `json:"at"`
	EventType string          `json:"event_type"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type subagentRunsOutput struct {
	Events       []subagentRunsEvent `json:"events"`
	NextSinceSeq int                 `json:"next_since_seq"`
}

func (parent *Session) callSubagentRuns(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if parent.IsClosed() {
		return toolErr("session_gone", "calling session has already terminated")
	}
	var in subagentRunsInput
	if err := json.Unmarshal(args, &in); err != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid subagent_runs args: %v", err))
	}
	if in.SessionID == "" {
		return toolErr("bad_request", "session_id is required")
	}

	if errFrame := parent.assertChildOf(ctx, in.SessionID); errFrame != nil {
		return errFrame, nil
	}

	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > subagentRunsHardCap {
		limit = subagentRunsHardCap
	}

	rows, err := parent.store.ListEvents(ctx, in.SessionID, store.ListEventsOpts{
		MinSeq: in.SinceSeq,
		Limit:  limit,
	})
	if err != nil {
		return toolErr("io", err.Error())
	}
	out := subagentRunsOutput{Events: make([]subagentRunsEvent, 0, len(rows))}
	maxSeq := in.SinceSeq
	for _, r := range rows {
		var payload json.RawMessage
		if r.Metadata != nil {
			if b, err := json.Marshal(r.Metadata); err == nil {
				payload = b
			}
		}
		out.Events = append(out.Events, subagentRunsEvent{
			Seq:       r.Seq,
			At:        r.CreatedAt,
			EventType: r.EventType,
			Payload:   payload,
		})
		if r.Seq > maxSeq {
			maxSeq = r.Seq
		}
	}
	if len(rows) >= limit {
		out.NextSinceSeq = maxSeq
	}
	return json.Marshal(out)
}
