package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// spawn_subagent — fan-out tool. Non-blocking; returns the new
// child session ids. Validates the whole batch atomically (depth
// cap, skill / role lookup) before issuing any parent.Spawn so the
// LLM never sees partial spawn state.

const spawnSubagentSchema = `{
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
			validation, err := describeSubagent(ctx, parent, e.Skill, e.Role)
			if err != nil {
				return toolErr("skill_not_found",
					fmt.Sprintf("subagents[%d]: %v", i, err))
			}
			switch validation {
			case extension.SubagentSkillFoundRoleMissing:
				return toolErr("role_not_found",
					fmt.Sprintf("subagents[%d]: role %q not declared in skill %q",
						i, e.Role, e.Skill))
			case extension.SubagentUnknown:
				// No advisor recognised the skill. Honour the legacy
				// "no advisor wired" short-circuit: when no extension
				// implements SubagentDescriber at all, we want to fall
				// through (treat as valid) so test fixtures without a
				// SkillManager don't break. hasSubagentDescriber tells
				// the two cases apart.
				if hasSubagentDescriber(parent) {
					return toolErr("skill_not_found",
						fmt.Sprintf("subagents[%d]: skill %q not found", i, e.Skill))
				}
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
		// child's goroutine is already started (parent.Spawn). Wait
		// on the settled channel so the child sees the task before
		// we move to the next batch entry; pre/post IsClosed checks
		// distinguish "delivered" from "child already gone".
		first := protocol.NewUserMessage(child.ID(), parent.agent.Participant(), e.Task)
		if !child.IsClosed() {
			<-child.Submit(ctx, first)
		}
		if child.IsClosed() {
			parent.logger.Warn("session: spawn_subagent: child rejected initial task",
				"parent", parent.id, "child", child.ID())
		}
	}
	return json.Marshal(out)
}
