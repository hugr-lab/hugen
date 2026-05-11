package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// spawn_mission is the root-only singular-spawn tool that root
// uses to delegate one user request to a mission session
// (phase 4.2.2 §4). Structurally identical to spawn_subagent with
// arity-1: root never fans out, it spawns one coordinator that
// then decomposes via spawn_wave. The split is deliberate — root
// having a singular spawn surface removes the "how many?"
// meta-decision that weak models repeatedly mishandle.
//
// For milestone β the handler accepts any skill argument; the
// mission.enabled validation against root's "Available missions"
// catalogue lands with milestone γ. Until then the model's choice
// is honoured verbatim and propagates to the spawned child as the
// `skill` field on its first SpawnSpec (existing path).

const spawnMissionSchema = `{
  "type": "object",
  "properties": {
    "goal":   {"type": "string", "description": "What the mission must accomplish. Becomes the mission's first user message."},
    "inputs": {"description": "Optional JSON the parent passes alongside the goal — schemas, anchors, prior context."},
    "skill":  {"type": "string", "description": "Skill that provides the mission coordinator pattern (e.g. analyst). Optional in β; mandatory once mission.enabled lands."},
    "role":   {"type": "string", "description": "Role within the skill. Optional."}
  },
  "required": ["goal"]
}`

type spawnMissionInput struct {
	Goal   string `json:"goal"`
	Inputs any    `json:"inputs,omitempty"`
	Skill  string `json:"skill,omitempty"`
	Role   string `json:"role,omitempty"`
}

func (parent *Session) callSpawnMission(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if parent.IsClosed() {
		return toolErr("session_gone", "calling session has already terminated")
	}
	var in spawnMissionInput
	if err := json.Unmarshal(args, &in); err != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid spawn_mission args: %v", err))
	}
	if strings.TrimSpace(in.Goal) == "" {
		return toolErr("bad_request", "goal is required")
	}

	// Reuse the batch validator + spawn loop by synthesising a
	// single-entry batch. Keeps spawn semantics (depth cap, skill /
	// role lookup, per-role intent hint, first-message delivery)
	// identical to spawn_subagent's path — there's exactly one
	// production-quality codepath for "spawn a child".
	batch, err := json.Marshal(spawnSubagentInput{
		Subagents: []spawnEntry{{
			Skill:  in.Skill,
			Role:   in.Role,
			Task:   in.Goal,
			Inputs: in.Inputs,
		}},
	})
	if err != nil {
		return toolErr("io", fmt.Sprintf("marshal batch shim: %v", err))
	}
	raw, err := parent.callSpawnSubagent(ctx, batch)
	if err != nil {
		return raw, err
	}
	// callSpawnSubagent returns either a tool_error envelope (bytes
	// + nil error) or an array of spawnSubagentResult. Pass through
	// errors as-is; for the success path, unwrap the singular row
	// and re-marshal so the LLM sees a single object, not a
	// one-element array.
	var rows []spawnSubagentResult
	if err := json.Unmarshal(raw, &rows); err != nil {
		// Was a tool_error envelope — forward verbatim.
		return raw, nil
	}
	if len(rows) != 1 {
		return toolErr("io",
			fmt.Sprintf("spawn_mission: expected 1 spawn result, got %d", len(rows)))
	}
	return json.Marshal(rows[0])
}
