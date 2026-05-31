package mission

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// MissionHook is one lifecycle-hook declaration: an invocation of an
// MCP tool already wired into the mission session (e.g. "bash:run",
// "python:run_script"). The runtime fires it through the same
// Resolve→Dispatch path the model's own tool calls take — minus the
// model — so a hook reuses the session's MCPs verbatim (bash's
// workspace confinement, python's per-session venv + Hugr token
// refresh, …). No new exec/sandbox surface.
//
// Tool is the fully-qualified tool name. Args is the raw argument
// object handed to the tool; every string value (recursively —
// inside arrays and nested objects too) is Go-template-rendered
// against a [hookView] before dispatch, so a hook can reference
// mission paths ({{.MissionDir}}, {{.MissionSkill}}) and runtime
// state ({{.Goal}}, {{.Roles}}, {{.Inputs.key}}).
type MissionHook struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args,omitempty"`
}

// IsZero reports whether the hook is unset (no tool declared). A
// stage's before/check hook is optional; an unset hook is a no-op.
func (h MissionHook) IsZero() bool { return strings.TrimSpace(h.Tool) == "" }

// hookView is the template context a hook's Args render against.
// Paths come from the mission session + skill; runtime state
// (goal, resolved inputs, worker roles) lets a check script reason
// about the plan it is validating.
type hookView struct {
	// MissionDir is the shared mission working directory — the same
	// absolute path for the mission session and every worker it
	// spawns (workers inherit the mission ancestor's workspace dir
	// verbatim). Hooks scaffold + validate artifacts under it.
	MissionDir string
	// MissionSkill is the absolute on-disk path of the dispatching
	// skill's bundle directory, so a scaffold hook can copy template
	// files out of it (cp {{.MissionSkill}}/templates/… {{.MissionDir}}/…).
	MissionSkill string
	// Goal is the mission goal string.
	Goal string
	// Inputs are the caller's resolved spawn inputs, stringified.
	Inputs map[string]string
	// Roles lists the worker role names the manifest declares.
	Roles []string
}

// hookOutcome is the decoded result of a fired hook. Failed folds
// the failure shapes the runtime cares about — an MCP is_error
// envelope or a non-zero process exit_code — into one gate signal
// the caller's stage gate consumes. Raw is the verbatim tool
// result for callers that want the full envelope.
type hookOutcome struct {
	Raw      json.RawMessage
	ExitCode int
	Failed   bool
	Reason   string
}

// runMissionHook resolves hook.Tool from the session's current tool
// snapshot, renders hook.Args against view, then Resolve+Dispatch —
// the exact pair session.dispatchToolCall uses. The runtime fires
// hooks on trust (skill-declared, not model-formulated), so the
// per-call RequiresApproval gate the model path applies is
// intentionally skipped here; permission tiers still apply via
// Resolve.
//
// A non-nil error means the hook could not be ATTEMPTED — tool
// missing from the snapshot, permission denied, or an args render
// failure; the caller treats that as a configuration fault. A nil
// error means the hook RAN: inspect outcome.Failed for the gate
// verdict (a provider dispatch error is folded into a failed
// outcome, not a runner error, so a check gate re-prompts rather
// than crashing the mission).
func runMissionHook(ctx context.Context, state extension.SessionState, hook MissionHook, view hookView) (*hookOutcome, error) {
	if hook.IsZero() {
		return nil, fmt.Errorf("mission: hook: empty tool name")
	}
	tm := state.Tools()
	if tm == nil {
		return nil, fmt.Errorf("mission: hook %q: session has no ToolManager", hook.Tool)
	}
	rendered, err := renderHookArgs(hook.Args, view)
	if err != nil {
		return nil, fmt.Errorf("mission: hook %q: render args: %w", hook.Tool, err)
	}
	snap, err := tm.Snapshot(ctx, state.SessionID())
	if err != nil {
		return nil, fmt.Errorf("mission: hook %q: tool snapshot: %w", hook.Tool, err)
	}
	var theTool tool.Tool
	for _, t := range snap.Tools {
		if t.Name == hook.Tool || tool.SanitizeName(t.Name) == hook.Tool {
			theTool = t
			break
		}
	}
	if theTool.Name == "" {
		return nil, fmt.Errorf("mission: hook %q: tool not in session snapshot", hook.Tool)
	}

	// Mirror session.dispatchToolCall: attach the calling state to
	// ctx so the provider can recover it (env injection, session
	// dir, …), then gate + dispatch with the canonical Tool.
	dispatchCtx := extension.WithSessionState(ctx, state)
	_, effective, err := tm.Resolve(dispatchCtx, theTool, rendered)
	if err != nil {
		return nil, fmt.Errorf("mission: hook %q: resolve: %w", hook.Tool, err)
	}
	raw, err := tm.Dispatch(dispatchCtx, theTool, effective)
	if err != nil {
		// A provider dispatch error (io / timeout / removed) is a hook
		// FAILURE the gate should react to, not a runner-level fault —
		// fold it into a failed outcome carrying the error text.
		return &hookOutcome{Failed: true, Reason: err.Error()}, nil
	}
	return decodeHookOutcome(raw), nil
}

// renderHookArgs Go-template-renders every string leaf in args
// against view and returns the marshalled JSON object the tool
// receives. nil / empty args render to "{}".
func renderHookArgs(args map[string]any, view hookView) (json.RawMessage, error) {
	if len(args) == 0 {
		return json.RawMessage("{}"), nil
	}
	rendered := make(map[string]any, len(args))
	for k, v := range args {
		rv, err := renderHookValue(v, view)
		if err != nil {
			return nil, fmt.Errorf("arg %q: %w", k, err)
		}
		rendered[k] = rv
	}
	return json.Marshal(rendered)
}

// renderHookValue renders a single args value: strings go through
// the template engine; arrays + objects recurse; everything else
// (numbers, bools, null) passes through untouched.
func renderHookValue(v any, view hookView) (any, error) {
	switch t := v.(type) {
	case string:
		return renderTemplateString(t, view)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			rv, err := renderHookValue(e, view)
			if err != nil {
				return nil, err
			}
			out[i] = rv
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			rv, err := renderHookValue(e, view)
			if err != nil {
				return nil, err
			}
			out[k] = rv
		}
		return out, nil
	default:
		return v, nil
	}
}

// renderTemplateString renders one string against view. Strings
// without a `{{` action are returned verbatim (fast path + lets a
// literal command skip template parsing). missingkey=error fails
// loud on a typo'd field so a broken hook surfaces at fire time
// rather than silently substituting "<no value>".
func renderTemplateString(s string, view hookView) (string, error) {
	if !strings.Contains(s, "{{") {
		return s, nil
	}
	tmpl, err := template.New("hook").Option("missingkey=error").Parse(s)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, view); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// decodeHookOutcome interprets the tool's raw result envelope into a
// pass/fail outcome. It recognises the two shapes the session MCPs
// emit: the structured `{exit_code, stdout, stderr, …}` bash/python
// run envelope (non-zero exit_code → failed) and the MCP error
// wrapper `{is_error: true, text}` (always failed). A result with
// neither marker (e.g. a hugr query payload, a plain `{text}`) is
// treated as a pass — the hook ran and the tool did not signal an
// error.
func decodeHookOutcome(raw json.RawMessage) *hookOutcome {
	out := &hookOutcome{Raw: raw}
	var env struct {
		ExitCode *int   `json:"exit_code"`
		IsError  bool   `json:"is_error"`
		Stderr   string `json:"stderr"`
		Text     string `json:"text"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		// Unparseable / non-object result — the hook still ran; leave
		// it as a pass so a tool with an exotic envelope doesn't wedge
		// the gate. Callers needing strictness inspect Raw.
		return out
	}
	if env.ExitCode != nil {
		out.ExitCode = *env.ExitCode
		if *env.ExitCode != 0 {
			out.Failed = true
			out.Reason = firstNonEmptyHook(env.Stderr, env.Text)
		}
	}
	if env.IsError {
		out.Failed = true
		if out.Reason == "" {
			out.Reason = firstNonEmptyHook(env.Text, env.Stderr)
		}
	}
	return out
}

func firstNonEmptyHook(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
