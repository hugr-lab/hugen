package session

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// spawn_wave is the mission-only atomic spawn+wait tool that
// fans out a set of workers and blocks until each terminates
// (phase 4.2.2 §5). Replaces the raw spawn_subagent +
// wait_subagents two-step on the mission's tool surface — one
// call per wave, no decisional overhead on "did I remember to
// wait?", uniform retry / cancel semantics.
//
// Implementation composes the existing batch spawn helper with
// the existing wait helper; there's no new internal mechanism.
// wait_timeout_ms maps onto a context deadline applied to the
// wait phase only (the spawn itself is fast and non-blocking).

const spawnWaveSchema = `{
  "type": "object",
  "properties": {
    "wave_label": {"type": "string", "description": "Short label for the wave — used for logging / plan commenting. Optional."},
    "subagents": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "properties": {
          "skill":  {"type": "string", "description": "Skill name providing the role."},
          "role":   {"type": "string", "description": "Role within the skill."},
          "task":   {"type": "string", "description": "Free-form prompt the worker sees as its first user message."},
          "inputs": {"description": "Optional JSON the mission passes to the worker."}
        },
        "required": ["task"]
      }
    },
    "wait_timeout_ms": {"type": "integer", "minimum": 0, "description": "Per-call deadline on the wait phase. 0 (default) uses the surrounding context's deadline."}
  },
  "required": ["subagents"]
}`

type spawnWaveInput struct {
	WaveLabel     string       `json:"wave_label,omitempty"`
	Subagents     []spawnEntry `json:"subagents"`
	WaitTimeoutMS int          `json:"wait_timeout_ms,omitempty"`
}

func (parent *Session) callSpawnWave(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if parent.IsClosed() {
		return toolErr("session_gone", "calling session has already terminated")
	}
	var in spawnWaveInput
	if err := json.Unmarshal(args, &in); err != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid spawn_wave args: %v", err))
	}
	if len(in.Subagents) == 0 {
		return toolErr("bad_request", "subagents must be a non-empty array")
	}

	// Spawn phase — delegate to the batch helper. On a validation
	// failure callSpawnSubagent returns a tool_error envelope; pass
	// it through verbatim so the mission sees the same envelope
	// shape it would from the raw tool.
	batch, err := json.Marshal(spawnSubagentInput{Subagents: in.Subagents})
	if err != nil {
		return toolErr("io", fmt.Sprintf("marshal batch: %v", err))
	}
	raw, err := parent.callSpawnSubagent(ctx, batch)
	if err != nil {
		return raw, err
	}
	var rows []spawnSubagentResult
	if err := json.Unmarshal(raw, &rows); err != nil {
		// tool_error envelope — forward as-is.
		return raw, nil
	}
	if len(rows) != len(in.Subagents) {
		return toolErr("io",
			fmt.Sprintf("spawn_wave: expected %d spawn results, got %d", len(in.Subagents), len(rows)))
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.SessionID)
	}

	// Wait phase — optional per-call deadline scoped to this wave.
	waitCtx := ctx
	if in.WaitTimeoutMS > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, time.Duration(in.WaitTimeoutMS)*time.Millisecond)
		defer cancel()
	}
	waitArgs, err := json.Marshal(waitSubagentsInput{IDs: ids})
	if err != nil {
		return toolErr("io", fmt.Sprintf("marshal wait args: %v", err))
	}
	waitRaw, err := parent.callWaitSubagents(waitCtx, waitArgs)
	if err != nil {
		return waitRaw, err
	}
	// On success callWaitSubagents returns a []waitResultRow; on
	// error (cancellation, bad request) it returns a tool_error
	// envelope. Either way the bytes are exactly what the mission
	// model should see, so pass through.
	return waitRaw, nil
}
