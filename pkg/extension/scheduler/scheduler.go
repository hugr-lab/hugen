// Package scheduler hosts the Phase 6 TaskManager per-session
// extension + the always-on `task_log_reap_stuck` system runner
// (see reap.go). The extension's 6.1b shape is intentionally narrow:
// it exposes the `task:create` tool so operators / the future
// `_task_builder` mission can persist task rows + the initial
// `planned` row into hub.db. Fire dispatch, drift detection, and
// the pause / resume / cancel / list surface land in 6.1c — those
// tools advertise here as stubs returning a structured "not_yet"
// error so manifests can already include them in their allow-lists.
package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	schedstore "github.com/hugr-lab/hugen/pkg/scheduler/store"
	"github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

const providerName = "task"

// Permission objects gated by Tier-1 operator config + Tier-2 Hugr
// role rules + Tier-3 per-user policies. The extension exposes the
// full surface; per-tier narrowing is the manifest's job.
const (
	PermCreate = "hugen:task:create"
	PermList   = "hugen:task:list"
	PermPause  = "hugen:task:pause"
	PermResume = "hugen:task:resume"
	PermCancel = "hugen:task:cancel"
)

// Extension is the per-session TaskManager. Constructed once at
// runtime boot with a [schedstore.TaskStore] + the agent's id;
// every session that loads the `scheduler` extension gets the same
// instance — task scope is enforced at the database layer through
// `tasks.owner_session_id`.
type Extension struct {
	store   schedstore.TaskStore
	skills  *skill.SkillManager
	agentID string
	logger  *slog.Logger
}

// NewExtension constructs the TaskManager extension. The
// SkillManager pointer is used by `task:create` to validate skill
// references + read `task.inputs_schema` for Phase 6.1c JSON-Schema
// validation; today the 6.1b path only confirms the skill exists.
func NewExtension(st schedstore.TaskStore, skills *skill.SkillManager, agentID string, logger *slog.Logger) *Extension {
	if logger == nil {
		logger = slog.Default()
	}
	return &Extension{store: st, skills: skills, agentID: agentID, logger: logger}
}

// Compile-time interface assertions.
var (
	_ extension.Extension = (*Extension)(nil)
	_ tool.ToolProvider   = (*Extension)(nil)
)

func (e *Extension) Name() string            { return providerName }
func (e *Extension) Lifetime() tool.Lifetime { return tool.LifetimePerAgent }

// InitState is a no-op for 6.1b: bootstrap-from-store + Runner
// registration land in 6.1c when fire dispatch is wired. Today the
// extension is a pure tool surface — task rows persist in hub.db
// and become observable via GraphQL queries, but the runtime does
// not fire them yet.
func (e *Extension) InitState(_ context.Context, _ extension.SessionState) error {
	return nil
}

const createSchema = `{
  "type": "object",
  "properties": {
    "skill_ref":     {"type": "string", "description": "Skill name to load on each fire (spawn kind). NULL/empty for wake kind."},
    "kind":          {"type": "string", "enum": ["wake", "spawn"], "description": "Fire delivery kind. 'wake' = synthetic UserMessage into owner session; 'spawn' = new cron session per fire."},
    "schedule_kind": {"type": "string", "enum": ["once_in", "once_at", "cron", "interval"], "description": "Schedule shape; semantics keyed by schedule_spec."},
    "schedule_spec": {"type": "string", "description": "Schedule expression — cron string, duration, RFC3339 timestamp etc. Interpreted by schedule_kind."},
    "initial_planned_at": {"type": "string", "description": "RFC3339 timestamp for the first fire. Required."},
    "name":          {"type": "string", "description": "Short label for UI / logs."},
    "description":   {"type": "string", "description": "User's intent in words. Optional."},
    "goal":          {"type": "string", "description": "Imperative one-line brief for spawn fires."},
    "wake_message":  {"type": "string", "description": "Synthetic UserMessage body for wake fires."},
    "allowed_tools": {"type": "array", "items": {"type": "string"}, "description": "Per-task tool allow-list (frozen here; CronApprovalPolicy enforces at each dispatch in 6.1c)."},
    "inputs":        {"type": "object", "description": "Structured inputs blob — validated against the skill manifest's task.inputs_schema in 6.1c."},
    "end_condition": {
      "type": "object",
      "properties": {
        "kind": {"type": "string", "enum": ["until_cancel", "count", "until"]},
        "spec": {"type": "string"}
      },
      "required": ["kind"]
    }
  },
  "required": ["kind", "schedule_kind", "schedule_spec", "initial_planned_at", "name", "end_condition"]
}`

// emptySchema is the placeholder schema for the 6.1c-deferred tools.
const emptySchema = `{"type": "object", "additionalProperties": true}`

// List implements [tool.ToolProvider].
func (e *Extension) List(_ context.Context) ([]tool.Tool, error) {
	return []tool.Tool{
		{
			Name:             providerName + ":create",
			Description:      "Create a scheduled task. Persists a `tasks` row + the initial `task_log` planned row atomically. Phase 6.1b — fire dispatch lands in 6.1c; rows created here remain visible via GraphQL but are not yet executed.",
			Provider:         providerName,
			PermissionObject: PermCreate,
			ArgSchema:        json.RawMessage(createSchema),
		},
		{
			Name:             providerName + ":list",
			Description:      "List tasks owned by the current session. Stub in 6.1b — returns not_yet_implemented; full surface lands in 6.1c alongside fire dispatch.",
			Provider:         providerName,
			PermissionObject: PermList,
			ArgSchema:        json.RawMessage(emptySchema),
		},
		{
			Name:             providerName + ":pause",
			Description:      "Pause a task. Stub in 6.1b — returns not_yet_implemented.",
			Provider:         providerName,
			PermissionObject: PermPause,
			ArgSchema:        json.RawMessage(emptySchema),
		},
		{
			Name:             providerName + ":resume",
			Description:      "Resume a paused task. Stub in 6.1b — returns not_yet_implemented.",
			Provider:         providerName,
			PermissionObject: PermResume,
			ArgSchema:        json.RawMessage(emptySchema),
		},
		{
			Name:             providerName + ":cancel",
			Description:      "Cancel a task. Stub in 6.1b — returns not_yet_implemented.",
			Provider:         providerName,
			PermissionObject: PermCancel,
			ArgSchema:        json.RawMessage(emptySchema),
		},
	}, nil
}

// Call implements [tool.ToolProvider].
func (e *Extension) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	short := stripProviderPrefix(name)
	switch short {
	case "create":
		return e.callCreate(ctx, args)
	case "list", "pause", "resume", "cancel":
		return notYetImplemented(short)
	default:
		return nil, fmt.Errorf("%w: task:%s", tool.ErrUnknownTool, short)
	}
}

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

// createInput is the shape `task:create` accepts. Field order matches
// the JSON Schema declared in createSchema above. Optional fields are
// omitted via pointer or zero-value sentinels.
type createInput struct {
	SkillRef         string                       `json:"skill_ref,omitempty"`
	Kind             string                       `json:"kind"`
	ScheduleKind     string                       `json:"schedule_kind"`
	ScheduleSpec     string                       `json:"schedule_spec"`
	InitialPlannedAt string                       `json:"initial_planned_at"`
	Name             string                       `json:"name"`
	Description      string                       `json:"description,omitempty"`
	Goal             string                       `json:"goal,omitempty"`
	WakeMessage      string                       `json:"wake_message,omitempty"`
	AllowedTools     []string                     `json:"allowed_tools,omitempty"`
	Inputs           map[string]any               `json:"inputs,omitempty"`
	EndCondition     schedstore.TaskEndCondition `json:"end_condition"`
}

// createOutput is the wire shape returned to the model on success.
type createOutput struct {
	TaskID           string `json:"task_id"`
	InitialPlannedAt string `json:"initial_planned_at"`
	Status           string `json:"status"`
}

func (e *Extension) callCreate(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if e.store == nil {
		return toolErr("not_yet_implemented", "TaskStore is not wired — Phase 6.1b deployment incomplete")
	}
	var in createInput
	if err := json.Unmarshal(args, &in); err != nil {
		return toolErr("invalid_args", fmt.Sprintf("decode: %v", err))
	}
	if in.Kind != schedstore.KindWake && in.Kind != schedstore.KindSpawn {
		return toolErr("invalid_args", fmt.Sprintf("kind must be wake|spawn (got %q)", in.Kind))
	}
	if in.Kind == schedstore.KindSpawn && in.SkillRef == "" {
		return toolErr("invalid_args", "spawn kind requires skill_ref")
	}
	if in.Kind == schedstore.KindWake && in.WakeMessage == "" {
		return toolErr("invalid_args", "wake kind requires wake_message")
	}
	if in.ScheduleSpec == "" {
		return toolErr("invalid_args", "schedule_spec required")
	}
	planned, err := time.Parse(time.RFC3339, in.InitialPlannedAt)
	if err != nil {
		return toolErr("invalid_args", fmt.Sprintf("initial_planned_at must be RFC3339 (got %q): %v", in.InitialPlannedAt, err))
	}
	if in.EndCondition.Kind == "" {
		return toolErr("invalid_args", "end_condition.kind required")
	}

	// Skill existence check (6.1b minimum). Full task.inputs_schema
	// JSON-Schema validation lands in 6.1c.
	if in.SkillRef != "" && e.skills != nil {
		sk, err := e.skills.Get(ctx, in.SkillRef)
		if err != nil {
			return toolErr("unknown_skill", fmt.Sprintf("skill %q not in catalog: %v", in.SkillRef, err))
		}
		if !sk.Manifest.Hugen.Task.Eligible {
			return toolErr("not_task_eligible", fmt.Sprintf("skill %q is not task-eligible (metadata.hugen.task.eligible)", in.SkillRef))
		}
		if sk.Manifest.Hugen.Task.Kind == skill.TaskKindMission {
			return toolErr("not_yet_implemented", "mission-shape tasks are reserved — MVP supports kind=worker only")
		}
	}

	state, ok := extension.SessionStateFromContext(ctx)
	if !ok {
		return toolErr("internal_error", "no session state in context")
	}

	taskID := newTaskID(e.agentID)
	row := schedstore.TaskRow{
		ID:             taskID,
		AgentID:        e.agentID,
		Kind:           in.Kind,
		Status:         schedstore.StatusActive,
		ScheduleKind:   in.ScheduleKind,
		OwnerSessionID: state.SessionID(),
		SkillRef:       in.SkillRef,
		Spec: schedstore.TaskSpec{
			Name:         in.Name,
			Description:  in.Description,
			ScheduleSpec: in.ScheduleSpec,
			EndCondition: in.EndCondition,
			Goal:         in.Goal,
			WakeMessage:  in.WakeMessage,
			AllowedTools: in.AllowedTools,
			Inputs:       in.Inputs,
		},
	}
	if err := e.store.OpenTask(ctx, row, planned.UTC()); err != nil {
		return toolErr("store_error", fmt.Sprintf("OpenTask: %v", err))
	}

	out := createOutput{
		TaskID:           taskID,
		InitialPlannedAt: planned.UTC().Format(time.RFC3339Nano),
		Status:           schedstore.StatusActive,
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("task:create: marshal response: %w", err)
	}
	return body, nil
}

// notYetImplemented is the shared stub response for the 6.1c-deferred
// tools. Returns a structured tool-error rather than a Go-level
// error so the model sees a clean signal it can route around.
func notYetImplemented(short string) (json.RawMessage, error) {
	return toolErr("not_yet_implemented", fmt.Sprintf("task:%s — full surface lands in Phase 6.1c alongside fire dispatch", short))
}

type toolErrorResponse struct {
	Error toolError `json:"error"`
}

type toolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func toolErr(code, msg string) (json.RawMessage, error) {
	body, err := json.Marshal(toolErrorResponse{Error: toolError{Code: code, Message: msg}})
	if err != nil {
		return nil, fmt.Errorf("task: marshal tool error: %w", err)
	}
	return body, nil
}

// newTaskID is the synthetic sortable id used for tasks rows. Shape
// `tsk_<agent>_<ts>_<rnd>` matches the conventions on memory_items /
// session_events.
func newTaskID(agentID string) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("tsk_%s_%d", agentID, time.Now().UnixNano())
	}
	return fmt.Sprintf("tsk_%s_%d_%s", agentID, time.Now().UnixNano(), hex.EncodeToString(b[:]))
}
