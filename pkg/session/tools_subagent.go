package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/skill"
)

// init registers the five US1 session-scoped tools into the
// package-level dispatch table set up in tools_provider.go. Per
// phase-4-spec §15 step 7 + contracts/tools-subagent.md these are the
// entries the LLM sees as session:spawn_subagent /
// session:wait_subagents / session:subagent_runs /
// session:subagent_cancel / session:parent_context.
func init() {
	sessionTools["spawn_subagent"] = sessionToolDescriptor{
		Name:             "spawn_subagent",
		Description:      "Spawn one or more sub-agent sessions. Non-blocking — returns child session ids immediately; results arrive asynchronously via wait_subagents.",
		PermissionObject: permObjectSubagentSpawn,
		ArgSchema:        json.RawMessage(spawnSubagentSchema),
		Handler:          (*Session).callSpawnSubagent,
	}
	sessionTools["wait_subagents"] = sessionToolDescriptor{
		Name:             "wait_subagents",
		Description:      "Block until each listed sub-agent produces a terminal result. Returns one row per id.",
		PermissionObject: permObjectSubagentWait,
		ArgSchema:        json.RawMessage(waitSubagentsSchema),
		Handler:          (*Session).callWaitSubagents,
	}
	sessionTools["subagent_runs"] = sessionToolDescriptor{
		Name:             "subagent_runs",
		Description:      "Paginated transcript pull-through for a sub-agent the calling session spawned.",
		PermissionObject: permObjectSubagentRead,
		ArgSchema:        json.RawMessage(subagentRunsSchema),
		Handler:          (*Session).callSubagentRuns,
	}
	sessionTools["subagent_cancel"] = sessionToolDescriptor{
		Name:             "subagent_cancel",
		Description:      "Cancel one of the calling session's sub-agents with a stated reason. Cascades to descendants via ctx.",
		PermissionObject: permObjectSubagentCancel,
		ArgSchema:        json.RawMessage(subagentCancelSchema),
		Handler:          (*Session).callSubagentCancel,
	}
	sessionTools["parent_context"] = sessionToolDescriptor{
		Name:             "parent_context",
		Description:      "Sub-agent's window into its direct parent's user-facing communication. Filtered to user/assistant messages.",
		PermissionObject: permObjectSubagentParentContext,
		ArgSchema:        json.RawMessage(parentContextSchema),
		Handler:          (*Session).callParentContext,
	}
}

// Permission objects per contracts/permission-objects.md §"Sub-agent
// system tools". These names are baked into the Tool descriptors
// returned by Manager.List so the 3-tier permission stack can gate
// each tool independently.
const (
	permObjectSubagentSpawn         = "hugen:subagent:spawn"
	permObjectSubagentWait          = "hugen:subagent:wait"
	permObjectSubagentRead          = "hugen:subagent:read"
	permObjectSubagentCancel        = "hugen:subagent:cancel"
	permObjectSubagentParentContext = "hugen:subagent:parent_context"
)

// JSON schemas for the five tools. Embedded as raw byte literals so
// the LLM provider layer can pass them through verbatim. Keep these
// in lock-step with contracts/tools-subagent.md.
const (
	spawnSubagentSchema = `{
  "type": "object",
  "properties": {
    "subagents": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "properties": {
          "skill":  {"type": "string", "description": "Skill name providing the role."},
          "role":   {"type": "string", "description": "Role within the skill."},
          "task":   {"type": "string", "description": "Free-form prompt the child sees as its first user message."},
          "inputs": {"description": "Optional JSON the parent passes to the child."}
        },
        "required": ["task"]
      }
    }
  },
  "required": ["subagents"]
}`

	waitSubagentsSchema = `{
  "type": "object",
  "properties": {
    "ids": {
      "type": "array",
      "items": {"type": "string"},
      "minItems": 1
    }
  },
  "required": ["ids"]
}`

	subagentRunsSchema = `{
  "type": "object",
  "properties": {
    "session_id": {"type": "string"},
    "since_seq":  {"type": "integer", "minimum": 0},
    "limit":      {"type": "integer", "minimum": 1, "maximum": 500}
  },
  "required": ["session_id"]
}`

	subagentCancelSchema = `{
  "type": "object",
  "properties": {
    "session_id": {"type": "string"},
    "reason":     {"type": "string"}
  },
  "required": ["session_id"]
}`

	parentContextSchema = `{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "Optional case-insensitive substring filter."},
    "from":  {"type": "string", "description": "Optional RFC3339 lower bound."},
    "to":    {"type": "string", "description": "Optional RFC3339 upper bound."},
    "limit": {"type": "integer", "minimum": 1, "maximum": 20}
  }
}`
)

// ---------- spawn_subagent ----------

type spawnSubagentInput struct {
	Subagents []spawnEntry `json:"subagents"`
}

type spawnEntry struct {
	Skill  string `json:"skill,omitempty"`
	Role   string `json:"role,omitempty"`
	Task   string `json:"task"`
	Inputs any    `json:"inputs,omitempty"`
}

type spawnSubagentResult struct {
	SessionID string `json:"session_id"`
	Depth     int    `json:"depth"`
}

func (parent *Session) callSpawnSubagent(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if parent.IsClosed() {
		return toolErr("session_gone", "calling session has already terminated")
	}
	var in spawnSubagentInput
	if err := json.Unmarshal(args, &in); err != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid spawn_subagent args: %v", err))
	}
	if len(in.Subagents) == 0 {
		return toolErr("bad_request", "subagents must be a non-empty array")
	}

	// Atomic batch validation — fail-fast on the first violation so
	// the parent doesn't see partial spawn state. Per
	// contracts/tools-subagent.md §spawn_subagent.
	maxDepth := DefaultMaxDepth
	if parent.deps != nil && parent.deps.MaxDepth > 0 {
		maxDepth = parent.deps.MaxDepth
	}
	for i, e := range in.Subagents {
		if strings.TrimSpace(e.Task) == "" {
			return toolErr("bad_request",
				fmt.Sprintf("subagents[%d].task is required", i))
		}
		if maxDepth > 0 && parent.depth+1 > maxDepth {
			return toolErr("depth_exceeded",
				fmt.Sprintf("parent depth %d + 1 exceeds runtime.max_depth %d",
					parent.depth, maxDepth))
		}
		if e.Skill != "" {
			role, ok, err := lookupSubAgentRole(ctx, parent, e.Skill, e.Role)
			if err != nil {
				return toolErr("skill_not_found",
					fmt.Sprintf("subagents[%d]: %v", i, err))
			}
			if !ok {
				return toolErr("role_not_found",
					fmt.Sprintf("subagents[%d]: role %q not declared in skill %q",
						i, e.Role, e.Skill))
			}
			if role != nil && !role.CanSpawnEffective() {
				// role.CanSpawn refers to whether the spawned child may
				// itself spawn — phase-4 §3 step 8. v1: enforce only at
				// the spawning side; the child re-checks on its own
				// spawn_subagent call.
				_ = role
			}
		}
	}

	// All entries valid — execute one parent.Spawn per request.
	out := make([]spawnSubagentResult, 0, len(in.Subagents))
	for i, e := range in.Subagents {
		spec := SpawnSpec{
			Skill:  e.Skill,
			Role:   e.Role,
			Task:   e.Task,
			Inputs: e.Inputs,
		}
		child, err := parent.Spawn(ctx, spec)
		if err != nil {
			// Spawn after validation should never fail except on
			// underlying I/O — surface as io error so the parent can
			// retry independently of the rest of the batch.
			return toolErr("io",
				fmt.Sprintf("subagents[%d]: spawn: %v", i, err))
		}
		out = append(out, spawnSubagentResult{
			SessionID: child.ID(),
			Depth:     child.depth,
		})

		// Deliver the task as the child's first user message so the
		// child's run-loop has something to drive a turn off of. The
		// child's goroutine is already started (parent.Spawn).
		first := protocol.NewUserMessage(child.ID(), parent.agent.Participant(), e.Task)
		if !child.Submit(ctx, first) {
			parent.logger.Warn("session: spawn_subagent: child rejected initial task",
				"parent", parent.id, "child", child.ID())
		}
	}
	return json.Marshal(out)
}

// lookupSubAgentRole resolves (skill, role) → *SubAgentRole through
// the parent's SkillManager. Returns:
//
//   - role=nil, ok=true, err=nil  → role omitted by caller; skill
//     exists but no specific role requested. Caller decides whether
//     to allow.
//   - role=nil, ok=false, err=nil → skill exists but role not
//     declared — surfaces as role_not_found.
//   - role=nil, ok=false, err!=nil → skill missing entirely —
//     surfaces as skill_not_found.
func lookupSubAgentRole(ctx context.Context, parent *Session, skillName, roleName string) (*skill.SubAgentRole, bool, error) {
	if parent.skills == nil {
		return nil, true, nil
	}
	all, err := parent.skills.List(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, s := range all {
		if s.Manifest.Name != skillName {
			continue
		}
		if roleName == "" {
			return nil, true, nil
		}
		for i := range s.Manifest.Hugen.SubAgents {
			r := s.Manifest.Hugen.SubAgents[i]
			if r.Name == roleName {
				return &r, true, nil
			}
		}
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("skill %q not found", skillName)
}

// ---------- wait_subagents ----------

type waitSubagentsInput struct {
	IDs []string `json:"ids"`
}

type waitResultRow struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	Result    string `json:"result,omitempty"`
	Reason    string `json:"reason,omitempty"`
	TurnsUsed int    `json:"turns_used,omitempty"`
}

func (parent *Session) callWaitSubagents(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if parent.IsClosed() {
		return toolErr("session_gone", "calling session has already terminated")
	}
	var in waitSubagentsInput
	if err := json.Unmarshal(args, &in); err != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid wait_subagents args: %v", err))
	}
	if len(in.IDs) == 0 {
		return toolErr("bad_request", "ids must be a non-empty array")
	}

	// First pass: collect any ids already terminal (cached in parent's
	// events as subagent_result OR the child's session_terminated)
	// before we register a feed. ListEvents lookup runs once per call;
	// the round-trip is cheap relative to a sub-agent's normal lifetime.
	collected := make(map[string]waitResultRow, len(in.IDs))
	if cached, err := drainCachedSubagentResults(ctx, parent, in.IDs); err == nil {
		for k, v := range cached {
			collected[k] = v
		}
	}

	pending := make(map[string]struct{}, len(in.IDs))
	for _, id := range in.IDs {
		if _, ok := collected[id]; ok {
			continue
		}
		pending[id] = struct{}{}
	}

	if len(pending) == 0 {
		return marshalWaitResults(in.IDs, collected)
	}

	// Register the active tool feed so the Run loop forwards matching
	// SubagentResult frames here. The loop reads activeToolFeed via
	// atomic load (session.go), so the store + clear is race-safe.
	feedCh := make(chan *protocol.SubagentResult, len(pending))
	feed := &ToolFeed{
		Consumes: func(k protocol.Kind) bool {
			return k == protocol.KindSubagentResult
		},
		Feed: func(f protocol.Frame) {
			sr, ok := f.(*protocol.SubagentResult)
			if !ok {
				return
			}
			select {
			case feedCh <- sr:
			default:
				// Buffered channel sized to len(pending); a full chan
				// means a duplicate result for an id we already drained
				// — drop it (subagent_result is exactly-once per child).
			}
		},
	}
	parent.activeToolFeed.Store(feed)
	defer parent.activeToolFeed.Store(nil)

	// Block until every pending id resolves, the parent's turn ctx
	// cancels (/cancel), or the call ctx fires.
	for len(pending) > 0 {
		select {
		case sr := <-feedCh:
			id := sr.Payload.SessionID
			if id == "" {
				id = sr.FromSessionID()
			}
			if _, want := pending[id]; !want {
				continue
			}
			row := waitResultRow{
				SessionID: id,
				Status:    statusFromReason(sr.Payload.Reason),
				Result:    sr.Payload.Result,
				Reason:    sr.Payload.Reason,
				TurnsUsed: sr.Payload.TurnsUsed,
			}
			collected[id] = row
			delete(pending, id)
			// Persist the consumed subagent_result into parent's events
			// so subsequent wait_subagents calls (or restart) see the
			// terminal state without rerunning the child.
			if err := parent.emit(ctx, sr); err != nil {
				parent.logger.Warn("session: wait_subagents: persist result",
					"parent", parent.id, "child", id, "err", err)
			}
		case <-ctx.Done():
			return toolErr("cancelled",
				fmt.Sprintf("wait_subagents aborted: %v", ctx.Err()))
		}
	}
	return marshalWaitResults(in.IDs, collected)
}

func marshalWaitResults(ids []string, collected map[string]waitResultRow) (json.RawMessage, error) {
	out := make([]waitResultRow, 0, len(ids))
	for _, id := range ids {
		if row, ok := collected[id]; ok {
			out = append(out, row)
		}
	}
	return json.Marshal(out)
}

// drainCachedSubagentResults walks parent's events for already-
// observed subagent_result rows matching ids. Used to short-circuit
// wait_subagents when the parent re-asks for ids that already
// resolved (e.g. polling pattern, restart-replay).
func drainCachedSubagentResults(ctx context.Context, parent *Session, ids []string) (map[string]waitResultRow, error) {
	want := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}
	rows, err := parent.store.ListEvents(ctx, parent.id, ListEventsOpts{Limit: 1000})
	if err != nil {
		return nil, err
	}
	out := make(map[string]waitResultRow)
	for _, r := range rows {
		if r.EventType != string(protocol.KindSubagentResult) {
			continue
		}
		var p protocol.SubagentResultPayload
		if r.Metadata != nil {
			if b, err := json.Marshal(r.Metadata); err == nil {
				_ = json.Unmarshal(b, &p)
			}
		}
		id := p.SessionID
		if id == "" {
			if v, ok := r.Metadata["__from_session"].(string); ok {
				id = v
			}
		}
		if _, match := want[id]; !match {
			continue
		}
		out[id] = waitResultRow{
			SessionID: id,
			Status:    statusFromReason(p.Reason),
			Result:    p.Result,
			Reason:    p.Reason,
			TurnsUsed: p.TurnsUsed,
		}
	}
	return out, nil
}

// statusFromReason maps a session_terminated.reason to the
// wait_subagents status enum exposed to the LLM. The status is the
// stable machine-readable handle; reason carries free-form context.
func statusFromReason(reason string) string {
	switch {
	case reason == protocol.TerminationCompleted:
		return "completed"
	case reason == protocol.TerminationHardCeiling:
		return "hard_ceiling"
	case reason == protocol.TerminationCancelCascade:
		return "cancel_cascade"
	case reason == protocol.TerminationRestartDied:
		return "restart_died"
	case strings.HasPrefix(reason, protocol.TerminationSubagentCancelPrefix):
		return "subagent_cancel"
	case strings.HasPrefix(reason, protocol.TerminationPanicPrefix):
		return "panic"
	default:
		return "completed"
	}
}

// ---------- subagent_runs ----------

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

const subagentRunsHardCap = 500

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

	rows, err := parent.store.ListEvents(ctx, in.SessionID, ListEventsOpts{
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

// assertChildOf returns a tool_error JSON when sessionID is not a
// direct child of the calling session (or doesn't exist). Used by
// subagent_runs + subagent_cancel to gate cross-session reads.
func (parent *Session) assertChildOf(ctx context.Context, sessionID string) json.RawMessage {
	if sessionID == parent.id {
		out, _ := toolErr("not_a_child", "session_id is the caller itself")
		return out
	}
	row, err := parent.store.LoadSession(ctx, sessionID)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			out, _ := toolErr("session_not_found", fmt.Sprintf("session %q not found", sessionID))
			return out
		}
		out, _ := toolErr("io", err.Error())
		return out
	}
	if row.ParentSessionID != parent.id {
		out, _ := toolErr("not_a_child",
			fmt.Sprintf("session %q is not a child of %q", sessionID, parent.id))
		return out
	}
	return nil
}

// ---------- subagent_cancel ----------

type subagentCancelInput struct {
	SessionID string `json:"session_id"`
	Reason    string `json:"reason,omitempty"`
}

type subagentCancelOutput struct {
	OK bool `json:"ok"`
}

func (parent *Session) callSubagentCancel(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if parent.IsClosed() {
		return toolErr("session_gone", "calling session has already terminated")
	}
	var in subagentCancelInput
	if err := json.Unmarshal(args, &in); err != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid subagent_cancel args: %v", err))
	}
	if in.SessionID == "" {
		return toolErr("bad_request", "session_id is required")
	}

	// Direct child lookup — Manager is root-only post-pivot 4 and a
	// session only ever cancels its OWN immediate children. Walking
	// the descendant tree is forbidden: each session is the source of
	// truth for its direct sub-tree and any deeper cancel must travel
	// through that owner.
	parent.childMu.Lock()
	child, live := parent.children[in.SessionID]
	parent.childMu.Unlock()

	if live {
		if child.IsClosed() {
			return json.Marshal(subagentCancelOutput{OK: true})
		}
		reason := protocol.TerminationSubagentCancelPrefix + strings.TrimSpace(in.Reason)
		// Phase 4.1b-pre stage B: cancel travels through the
		// SessionClose Frame so the child's Run loop drives its own
		// teardown (writes session_terminated with the prefixed
		// reason; emits subagent_result back to parent via handleExit's
		// subagentResultSent gate).
		closeFrame := protocol.NewSessionClose(child.id, parent.agent.Participant(), reason)
		child.Submit(ctx, closeFrame)
		select {
		case <-child.Done():
		case <-ctx.Done():
			return toolErr("cancelled", ctx.Err().Error())
		}
		// parent.children cleanup: handleSubagentResult won't fire for
		// the cancel-path subagent_result (it arrived during parent's
		// teardown? no — parent is alive). Actually parent's Run loop
		// will receive the subagent_result triggered by child.handleExit
		// and run handleSubagentResult naturally; that will see the
		// child entry already gone if we delete it here. To keep the
		// invariant simple we let handleSubagentResult do the cleanup
		// when the result arrives.
		return json.Marshal(subagentCancelOutput{OK: true})
	}

	// Not in the live children map — either already-terminal (the
	// goroutine exited and the deregister callback removed it) or
	// not a child of caller at all. Confirm direct parentage in the
	// store; not_a_child / session_not_found surface the wiring error,
	// otherwise the cancel is idempotent ok=true.
	if errFrame := parent.assertChildOf(ctx, in.SessionID); errFrame != nil {
		return errFrame, nil
	}
	return json.Marshal(subagentCancelOutput{OK: true})
}

// ---------- parent_context ----------

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

const parentContextHardCap = 20

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

	rows, err := caller.store.ListEvents(ctx, parentID, ListEventsOpts{Limit: 1000})
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
func parentContextRole(r EventRow) (string, bool) {
	switch r.EventType {
	case string(protocol.KindUserMessage), EventTypeHumanMessageReceived:
		return "user", true
	case string(protocol.KindAgentMessage), EventTypeAssistantMessageSent:
		return "assistant", true
	}
	return "", false
}
