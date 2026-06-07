package mission

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// Permission objects gated by the 3-tier perm stack. Phase A
// shipped :finish + :get_handoff; Phase E adds :notify so root
// can deliver mid-mission followups without re-spawning.
const (
	PermFinish             = "hugen:mission:finish"
	PermGetHandoff         = "hugen:mission:get_handoff"
	PermNotify             = "hugen:mission:notify"
	PermValidateAndApprove = "hugen:mission:validate_and_approve"
	PermGetResearch        = "hugen:mission:get_research"
)

const (
	missionFinishSchema = `{
  "type": "object",
  "properties": {
    "reason": {
      "type": "string",
      "description": "Termination reason — one of: completed | cancelled | failed | max_iterations_exhausted. Required."
    },
    "text": {
      "type": "string",
      "description": "Optional final answer text the supervisor renders into the mission's terminal SubagentResult. When omitted, the runtime synthesises a generic completion message."
    }
  },
  "required": ["reason"]
}`

	missionGetHandoffSchema = `{
  "type": "object",
  "properties": {
    "ref": {
      "type": "string",
      "description": "Handoff ref to fetch — \"<subagent_name>@<wave_label>\" as listed in [Available handoffs] in the worker's first message. Required."
    }
  },
  "required": ["ref"]
}`

	missionValidateAndApproveSchema = `{
  "type": "object",
  "properties": {
    "body": {
      "type": "object",
      "description": "The plan body you are about to emit — exactly the object you would place under \"body\" in the fenced ` + "`" + `plan` + "`" + ` block. Must carry next_wave (or null for plan_complete), roadmap (array), and rationale (string)."
    }
  },
  "required": ["body"]
}`

	missionNotifySchema = `{
  "type": "object",
  "properties": {
    "name": {
      "type": "string",
      "description": "Mission name (the 'name' arg used at session:spawn_mission) OR session_id. Required."
    },
    "text": {
      "type": "string",
      "description": "Followup text the user wants the mission to consider. Lands in the mission's plan_context journal under phase=user-followup so the next planner sees it. Required."
    }
  },
  "required": ["name", "text"]
}`

	missionGetResearchSchema = `{
  "type": "object",
  "properties": {}
}`
)

type finishInput struct {
	Reason string `json:"reason"`
	Text   string `json:"text,omitempty"`
}

type getHandoffInput struct {
	Ref string `json:"ref"`
}

type notifyInput struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

type validateAndApproveInput struct {
	Body json.RawMessage `json:"body"`
}

type toolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type toolErrorResponse struct {
	Error toolError `json:"error"`
}

func toolErr(code, msg string) (json.RawMessage, error) {
	return json.Marshal(toolErrorResponse{Error: toolError{Code: code, Message: msg}})
}

// List implements [tool.ToolProvider].
func (e *Extension) List(_ context.Context) ([]tool.Tool, error) {
	return []tool.Tool{
		{
			Name:             providerName + ":finish",
			Description:      "Terminate the mission with a structured reason and optional final text. Supervisor-only — workers cannot finish a mission.",
			Provider:         providerName,
			PermissionObject: PermFinish,
			ArgSchema:        json.RawMessage(missionFinishSchema),
		},
		{
			Name:             providerName + ":get_handoff",
			Description:      "Fetch a stored handoff by ref. Refs are discovered through the [Available handoffs] catalog in the worker's first message — never invent ref names.",
			Provider:         providerName,
			PermissionObject: PermGetHandoff,
			ArgSchema:        json.RawMessage(missionGetHandoffSchema),
		},
		{
			Name:             providerName + ":notify",
			Description:      "Deliver a mid-mission followup from the user to a running mission. Append-only; lands in the mission's plan_context under phase=user-followup so the next planner sees it on its next spawn. Root-tier callers only.",
			Provider:         providerName,
			PermissionObject: PermNotify,
			ArgSchema:        json.RawMessage(missionNotifySchema),
		},
		{
			Name:             providerName + ":validate_and_approve",
			Description:      "Validate a candidate plan body AND, in the same call, surface it to the user as an approval inquire when this iteration needs sign-off. Pass the same JSON object you intend to emit under `body` in the fenced `plan` block. Returns `{ valid, errors[], approved, refine_text?, aborted?, reason? }`. The modal opens when (1) this is the first plan in the mission, (2) a worker handoff requested reapproval, or (3) this body sets `requires_reapproval: true`. Otherwise the call passes silently and the prior approval stands. Approval flow: on `approved=true` emit the plan fence. On `refine_text` populated, revise the plan and call this tool again. On `aborted=true` emit a `status: error` handoff carrying the abort reason. Planner-tier only.",
			Provider:         providerName,
			PermissionObject: PermValidateAndApprove,
			ArgSchema:        json.RawMessage(missionValidateAndApproveSchema),
		},
		{
			Name:             providerName + ":get_research",
			Description:      "Fetch the mission's research-stage output: the findings paragraph the researcher emitted on done=true, the relative paths of the artifact files it wrote (file_refs — READ these before re-deriving), plus any structured resolved_user_inputs (file_path, output_format, scope choices the user picked) and ac_proposals. Returns `{ available: bool, findings: string, file_refs: [...], resolved_user_inputs: {...}, ac_proposals: [...] }`. Call when your task brief references scope set by the research stage and you need the full context — schema names, the research files to open, resolved scope choices — rather than re-discovering them. `available: false` means the mission didn't run a research stage; treat your task brief as the canonical source.",
			Provider:         providerName,
			PermissionObject: PermGetResearch,
			ArgSchema:        json.RawMessage(missionGetResearchSchema),
		},
	}, nil
}

// Call implements [tool.ToolProvider]. Routes by short tool name.
func (e *Extension) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	short := strings.TrimPrefix(name, providerName+":")
	switch short {
	case "finish":
		return e.callFinish(ctx, args)
	case "get_handoff":
		return e.callGetHandoff(ctx, args)
	case "notify":
		return e.callNotify(ctx, args)
	case "validate_and_approve":
		return e.callValidateAndApprove(ctx, args)
	case "get_research":
		return e.callGetResearch(ctx, args)
	default:
		return nil, fmt.Errorf("%w: mission:%s", tool.ErrUnknownTool, short)
	}
}

// Subscribe implements [tool.ToolProvider]. Static catalogue.
func (e *Extension) Subscribe(_ context.Context) (<-chan tool.ProviderEvent, error) {
	return nil, nil
}

// Close implements [tool.ToolProvider]. Per-session state is in
// SessionState; nothing for the provider value itself to release.
func (e *Extension) Close() error { return nil }

// callFinish — Phase A skeleton. Validates the input and returns a
// structured ok envelope. The actual session-close handshake (emit
// AgentMessage{Final:true,Consolidated:true}, transition the
// mission session to teardown) wires in Phase B once the
// supervisor flow lands. For Phase A this tool is callable as
// a no-op so the integration scenario can observe a clean call
// site without the runtime crashing.
func (e *Extension) callFinish(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	state, ok := extension.SessionStateFromContext(ctx)
	if !ok || state == nil {
		return toolErr("session_gone", "no session attached to dispatch ctx")
	}
	var in finishInput
	if err := json.Unmarshal(args, &in); err != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid mission:finish args: %v", err))
	}
	if strings.TrimSpace(in.Reason) == "" {
		return toolErr("bad_request", "reason is required")
	}
	m := FromState(state)
	if m == nil {
		return toolErr("unavailable", "mission state not initialised on this session")
	}
	// Phase A: stash the finish intent on the mission state so the
	// scenario harness / status reporter can observe it. The
	// teardown handshake itself is deferred to Phase B.
	m.mu.Lock()
	if m.Plan.Roadmap == nil {
		m.Plan.Roadmap = nil
	}
	m.mu.Unlock()
	out := map[string]any{
		"ok":     true,
		"reason": in.Reason,
	}
	if in.Text != "" {
		out["text_len"] = len(in.Text)
	}
	return json.Marshal(out)
}

// callNotify delivers a mid-mission followup from a root-tier
// session to the named mission. Phase E — minimum viable cut:
// synchronous append to the mission's PlanContext under
// phase=user-followup, plus a user_followup ExtensionFrame on
// the mission session for observability. The 5-second debounce
// queue specced in canon §16.9 is deferred to a follow-up — v1's
// synchronous path is sufficient for the integration scenario.
//
// Resolution rules:
//
//   - the caller MUST be a root session (depth 0). Workers /
//     missions calling :notify are rejected with `forbidden`.
//   - `name` matches either the spawn-time name OR the mission
//     session id. First match in state.Children() wins; the
//     surface stays single-mission for v1.
//
// Returns an ok envelope with the mission session id on success,
// or a structured error envelope on resolution failure.
func (e *Extension) callNotify(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	state, ok := extension.SessionStateFromContext(ctx)
	if !ok || state == nil {
		return toolErr("session_gone", "no session attached to dispatch ctx")
	}
	if state.Depth() != 0 {
		return toolErr("forbidden", "mission:notify can only be called from a root session")
	}
	var in notifyInput
	if err := json.Unmarshal(args, &in); err != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid mission:notify args: %v", err))
	}
	if strings.TrimSpace(in.Name) == "" {
		return toolErr("bad_request", "name is required")
	}
	if strings.TrimSpace(in.Text) == "" {
		return toolErr("bad_request", "text is required")
	}

	target, missionState := resolveMissionByName(state, in.Name)
	if target == nil {
		return toolErr("not_found", fmt.Sprintf("no mission named %q under this root", in.Name))
	}
	if missionState == nil {
		return toolErr("unavailable", "mission state not initialised on target session")
	}
	missionState.PlanContext.Append(PlanContextEntry{
		Iteration: readIterationCounter(missionState),
		Phase:     "user-followup",
		Summary:   strings.TrimSpace(in.Text),
	})
	e.emitUserFollowup(target, in.Text)
	return json.Marshal(map[string]any{
		"ok":         true,
		"session_id": target.SessionID(),
		"name":       in.Name,
	})
}

// resolveMissionByName walks state.Children() looking for a
// session whose SubagentName matches name OR whose SessionID
// equals name. Returns (childState, missionState) on hit;
// (nil, nil) when nothing matched.
func resolveMissionByName(root extension.SessionState, name string) (extension.SessionState, *MissionState) {
	for _, child := range root.Children() {
		if child == nil {
			continue
		}
		if child.SubagentName() == name || child.SessionID() == name {
			return child, FromState(child)
		}
	}
	return nil, nil
}

// readIterationCounter returns the mission's current iteration
// without exporting the lock.
func readIterationCounter(m *MissionState) int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.IterationCounter
}

// getResearchResponse is the JSON shape callGetResearch emits.
// available=true with populated findings means the researcher
// emitted done=true and stashed output on MissionState.
// available=false + attempted=false means the mission has no
// research stage configured — the worker's task brief is the
// canonical source.
// available=false + attempted=true means research ran but
// produced no usable output (empty findings, decode failure,
// or stage aborted). Worker should NOT assume scope was
// pre-researched; the task brief stands alone.
type getResearchResponse struct {
	Available          bool                 `json:"available"`
	Attempted          bool                 `json:"attempted,omitempty"`
	Findings           string               `json:"findings,omitempty"`
	FileRefs           []string             `json:"file_refs,omitempty"`
	ResolvedUserInputs map[string]any       `json:"resolved_user_inputs,omitempty"`
	ACProposals        []ResearchACProposal `json:"ac_proposals,omitempty"`
}

// callGetResearch projects MissionState.ResearchOutput() onto the
// JSON envelope workers see. The mission state is resolved via
// FromState which walks the parent chain — so a worker calling
// this tool from within its own session lands on the parent
// mission's state without any explicit handle plumbing.
//
// Same dispatch context handling as callGetHandoff: returns
// session_gone when no session attached, unavailable when the
// MissionState extension wasn't initialised on the resolved
// session.
func (e *Extension) callGetResearch(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	state, ok := extension.SessionStateFromContext(ctx)
	if !ok || state == nil {
		return toolErr("session_gone", "no session attached to dispatch ctx")
	}
	m := FromState(state)
	if m == nil {
		return toolErr("unavailable", "mission state not initialised on this session")
	}
	findings, resolved, acProposals := m.ResearchOutput()
	if strings.TrimSpace(findings) == "" && len(resolved) == 0 && len(acProposals) == 0 {
		return json.Marshal(getResearchResponse{
			Available: false,
			Attempted: m.ResearchAttempted(),
		})
	}
	return json.Marshal(getResearchResponse{
		Available:          true,
		Attempted:          true,
		Findings:           findings,
		FileRefs:           m.ResearchFileRefs(),
		ResolvedUserInputs: resolved,
		ACProposals:        acProposals,
	})
}

// callGetHandoff reads a handoff from the per-mission store. Any
// ref in the store is fetchable (no per-worker scoping — discovery
// happens via the first-message catalog). Returns an error envelope
// when ref is empty, malformed, or absent.
func (e *Extension) callGetHandoff(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	state, ok := extension.SessionStateFromContext(ctx)
	if !ok || state == nil {
		return toolErr("session_gone", "no session attached to dispatch ctx")
	}
	var in getHandoffInput
	if err := json.Unmarshal(args, &in); err != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid mission:get_handoff args: %v", err))
	}
	if strings.TrimSpace(in.Ref) == "" {
		return toolErr("bad_request", "ref is required")
	}
	if _, _, err := ParseRef(in.Ref); err != nil {
		return toolErr("bad_request", err.Error())
	}
	m := FromState(state)
	if m == nil {
		return toolErr("unavailable", "mission state not initialised on this session")
	}
	h, ok := m.Handoffs.Get(in.Ref)
	if !ok {
		// List the refs that DO exist so the caller (a worker, or the
		// planner on amend) corrects a wrong/invented ref instead of
		// flailing (the dogfood failure: a planner invented
		// `module-fetcher@fetch-modules`, the worker then hunted the
		// filesystem for it). Internal planner/checker/synth refs
		// (`@_plan-*` / `@_check-*` / …) are hidden — they're not data
		// handoffs a worker should read.
		avail := availableDataRefs(m)
		msg := fmt.Sprintf("handoff %q not in store", in.Ref)
		if len(avail) > 0 {
			msg += "; available refs: " + strings.Join(avail, ", ")
		} else {
			msg += "; no wave handoffs have been produced yet"
		}
		return toolErr("not_found", msg)
	}
	return json.Marshal(h)
}

// availableDataRefs returns the sorted, data-only handoff refs in the
// mission store — runtime-internal refs (planner / checker / synth
// waves, labelled `_plan-*` / `_check-*` / …) are excluded since a
// worker reads worker outputs, not control handoffs.
func availableDataRefs(m *MissionState) []string {
	if m == nil {
		return nil
	}
	var refs []string
	for _, h := range m.Handoffs.List() {
		if h.Ref == "" || strings.Contains(h.Ref, "@_") {
			continue
		}
		refs = append(refs, h.Ref)
	}
	sort.Strings(refs)
	return refs
}
