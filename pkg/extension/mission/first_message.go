package mission

import (
	"encoding/json"
	"strings"
)

// buildWorkerFirstMessage composes the user-message body the mission
// ext sends as a freshly-spawned worker's first turn input. It
// prepends an [Inputs from parent] section when the planner attached
// non-trivial `inputs` to the wave subagent spec — without that
// block the worker only sees the free-form task text and has no
// concrete values for parameters the planner lifted from
// `[Resolved user inputs]` (file_path, output_format, scope picks,
// chart picks, …).
//
// Owner: mission ext. The section name is the one documented in
// assets/prompts/mission/worker_contract.tmpl — workers grep for
// "[Inputs from parent]" verbatim, so the literal MUST match the
// template.
//
// Trivial inputs (nil, empty object, empty array, JSON null,
// empty string) degrade to the bare task — no header noise on
// workers that don't carry parameters.
//
// Returns task verbatim when inputs are trivial. The task string
// may already carry a [Resolved depends_on] prefix (executor.RunWave
// prepends one when the wave declared depends_on refs); both blocks
// then coexist in the final message, [Inputs from parent] sitting
// above the resolved-deps prefix that opens [Task].
func buildWorkerFirstMessage(task string, inputs any) string {
	if inputs == nil {
		return task
	}
	body, err := json.MarshalIndent(inputs, "", "  ")
	if err != nil {
		return task
	}
	trimmed := strings.TrimSpace(string(body))
	switch trimmed {
	case "", "null", "{}", "[]", `""`:
		return task
	}
	return "[Inputs from parent]\n" + trimmed + "\n\n[Task]\n" + task
}
