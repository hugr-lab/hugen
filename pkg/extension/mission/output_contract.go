package mission

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// OutputContractKind discriminates the four structured shapes a
// mission-aware subagent can emit. Workers emit kind=handoff;
// planners kind=plan; checkers kind=verdict; synthesizers
// kind=synthesis.
//
// Validation depth in v1 is "basic shape check" only — the parser
// confirms the kind is one of the four known values and that the
// kind-specific required fields are present. JSON Schema validation
// of body shape is deferred to Phase I (spec §4).
type OutputContractKind string

const (
	KindHandoff   OutputContractKind = "handoff"
	KindPlan      OutputContractKind = "plan"
	KindVerdict   OutputContractKind = "verdict"
	KindSynthesis OutputContractKind = "synthesis"
	// KindResearch is the Phase 5.x — B15 research-stage shape.
	// Body is the typed ResearchOutput (clarifications,
	// resolved_user_inputs, done, ac_proposals, findings,
	// memory_summary). Emitted by the research role; consumed by
	// the runtime's research-stage loop before the planner runs.
	KindResearch OutputContractKind = "research"
)

// Known returns true when k is one of the five declared kinds.
func (k OutputContractKind) Known() bool {
	switch k {
	case KindHandoff, KindPlan, KindVerdict, KindSynthesis, KindResearch:
		return true
	default:
		return false
	}
}

// OutputContract is the per-role contract attached to every worker
// in the mission manifest. Workers[*].output_contract.kind names
// the shape; optional Schema (deferred to Phase I) would tighten
// validation; Retries caps how many times the executor re-prompts
// a worker whose handoff fails validation.
type OutputContract struct {
	Kind OutputContractKind `json:"kind" yaml:"kind"`

	// Schema is the optional JSON Schema Draft 2020-12 fragment
	// validating Body for kind=handoff/synthesis. Phase I —
	// ignored by the v1 parser; field is kept so phase-G manifests
	// can declare schemas that future v2 validates.
	Schema json.RawMessage `json:"schema,omitempty" yaml:"schema,omitempty"`

	// Retries caps the number of times the executor re-prompts a
	// worker whose handoff fails parse. Zero falls back to the
	// runtime default (1). Cap: 3 per canon §16.2.
	Retries int `json:"retries,omitempty" yaml:"retries,omitempty"`
}

// ParseError is returned when the parser cannot extract a valid
// handoff from a worker's terminal message. The Reason is rendered
// into the retry-prompt's [output_contract_violation] system message
// so the worker can self-correct.
type ParseError struct {
	Reason string
	Body   string
}

func (e *ParseError) Error() string {
	if e == nil {
		return ""
	}
	return "output_contract: " + e.Reason
}

// handoffBlockRe matches a fenced YAML or JSON block tagged
// `handoff` / `plan` / `verdict` / `synthesis` / `research`.
// Tolerant to surrounding prose — the parser scans for the first
// matching block; everything outside is treated as commentary.
//
// (?s) — dot matches newline.
var handoffBlockRe = regexp.MustCompile(
	"(?s)```(handoff|plan|verdict|synthesis|research)\\s*\\n(.+?)```",
)

// jsonBlockRe matches a fenced ```json``` block, used as a fallback
// when the worker forgot the explicit kind tag — we try to parse
// JSON and infer kind from a top-level "kind" field.
var jsonBlockRe = regexp.MustCompile("(?s)```json\\s*\\n(.+?)```")

// ParseHandoff extracts a Handoff from raw, the worker's terminal
// AgentMessage text. Recognises:
//
//  1. ` ```handoff ... ``` ` / ` ```plan ... ``` ` / ` ```verdict ... ``` `
//     / ` ```synthesis ... ``` ` fenced YAML blocks (preferred).
//  2. ` ```json ... ``` ` fenced JSON block with a top-level
//     "kind": "<one of four>".
//
// The parser is intentionally permissive on payload shape — it
// validates only the required fields per kind. Field-level schema
// validation is Phase I.
//
// Returns (Handoff, nil) on success; (zero, *ParseError) when no
// recognisable block is present or required fields are missing.
//
// Ref / Subagent / CreatedAt are NOT set by the parser — the
// executor fills them in once it knows the worker's identity and
// wave context.
func ParseHandoff(raw string) (Handoff, error) {
	if strings.TrimSpace(raw) == "" {
		return Handoff{}, &ParseError{Reason: "empty body"}
	}

	if m := handoffBlockRe.FindStringSubmatch(raw); m != nil {
		kind := OutputContractKind(m[1])
		body := strings.TrimSpace(m[2])
		return decodeHandoff(kind, body)
	}

	if m := jsonBlockRe.FindStringSubmatch(raw); m != nil {
		body := strings.TrimSpace(m[1])
		var probe struct {
			Kind OutputContractKind `json:"kind"`
		}
		if err := json.Unmarshal([]byte(body), &probe); err == nil && probe.Kind.Known() {
			return decodeHandoff(probe.Kind, body)
		}
	}

	return Handoff{}, &ParseError{
		Reason: "no fenced handoff block found (expected ```handoff|plan|verdict|synthesis ... ```)",
		Body:   truncateBody(raw, 256),
	}
}

// decodeHandoff parses the body bytes (YAML- or JSON-shaped) into a
// Handoff value for the named kind. Tries JSON first (strict) and
// falls back to a lenient YAML-ish decode that maps the canonical
// keys we care about.
func decodeHandoff(kind OutputContractKind, body string) (Handoff, error) {
	if !kind.Known() {
		return Handoff{}, &ParseError{Reason: fmt.Sprintf("unknown kind %q", kind)}
	}

	var generic map[string]any
	if err := json.Unmarshal([]byte(body), &generic); err != nil {
		return Handoff{}, &ParseError{
			Reason: fmt.Sprintf("decode body as JSON: %v", err),
			Body:   truncateBody(body, 256),
		}
	}

	h := Handoff{Kind: kind}
	if v, ok := generic["status"].(string); ok {
		h.Status = strings.TrimSpace(v)
	}
	if v, ok := generic["reason"].(string); ok {
		h.Reason = strings.TrimSpace(v)
	}
	if v, ok := generic["memory_summary"].(string); ok {
		h.MemorySummary = strings.TrimSpace(v)
	}
	if v, ok := generic["body"]; ok {
		h.Body = v
	}
	if raw, ok := generic["satisfies"].([]any); ok {
		for _, e := range raw {
			if s, ok := e.(string); ok {
				s = strings.TrimSpace(s)
				if s != "" {
					h.Satisfies = append(h.Satisfies, s)
				}
			}
		}
	}

	if err := validateRequired(kind, h, generic); err != nil {
		return Handoff{}, err
	}
	return h, nil
}

// validateRequired enforces the per-kind required-field discipline.
//
// kind=handoff/synthesis: status required; reason required when
// status != "ok".
//
// kind=plan: status required; body must be an object carrying ALL
// three top-level keys — `next_wave`, `roadmap`, `rationale` — per
// spec §Phase B. `next_wave` may be JSON-null to signal "planner is
// done" (plan_complete). When present, `next_wave` must carry a
// non-empty `label` plus at least one entry in `subagents` — the
// shape the Plan Executor expects to run.
//
// kind=verdict: status + body.decision required.
func validateRequired(kind OutputContractKind, h Handoff, raw map[string]any) error {
	if h.Status == "" {
		return &ParseError{Reason: "status is required"}
	}
	if h.Status != "ok" && h.Reason == "" {
		return &ParseError{Reason: "reason is required when status != \"ok\""}
	}
	switch kind {
	case KindPlan:
		body, _ := raw["body"].(map[string]any)
		if body == nil {
			return &ParseError{Reason: "kind=plan requires a body object with next_wave, roadmap, rationale"}
		}
		if _, ok := body["next_wave"]; !ok {
			return &ParseError{Reason: "kind=plan requires body.next_wave (use null to signal plan_complete)"}
		}
		if _, ok := body["roadmap"]; !ok {
			return &ParseError{Reason: "kind=plan requires body.roadmap (empty array allowed)"}
		}
		if _, ok := body["rationale"]; !ok {
			return &ParseError{Reason: "kind=plan requires body.rationale"}
		}
		// next_wave may be null (plan_complete) or a wave object.
		if nw := body["next_wave"]; nw != nil {
			wave, ok := nw.(map[string]any)
			if !ok {
				return &ParseError{Reason: "kind=plan: body.next_wave must be an object or null"}
			}
			label, _ := wave["label"].(string)
			if strings.TrimSpace(label) == "" {
				return &ParseError{Reason: "kind=plan: body.next_wave.label is required"}
			}
			subs, _ := wave["subagents"].([]any)
			if len(subs) == 0 {
				return &ParseError{Reason: "kind=plan: body.next_wave.subagents must list at least one worker"}
			}
			// Phase I.26 → Phase 5.x B11 §3.2: mission_goal stays
			// required (planner's current restatement of intent).
			// Acceptance criteria now flow as diffs — ac_add / ac_update
			// — instead of a re-emitted full list. Either, both, or
			// neither may be present on a given iteration; the runtime
			// applies the diff over `state.AC` and enforces the
			// "first-iter must populate AC" gate at the planner-loop
			// level (where it sees both the manifest seed and the
			// planner's diff together).
			goal, _ := body["mission_goal"].(string)
			if strings.TrimSpace(goal) == "" {
				return &ParseError{Reason: "kind=plan: body.mission_goal is required (planner's current restatement of what the mission delivers)"}
			}
			if err := validatePlanACDiffWire(body); err != nil {
				return err
			}
		}
	case KindVerdict:
		body, _ := raw["body"].(map[string]any)
		if body == nil {
			return &ParseError{Reason: "kind=verdict requires a body object with decision"}
		}
		decision, _ := body["decision"].(string)
		if decision == "" {
			return &ParseError{Reason: "kind=verdict requires body.decision"}
		}
		if !VerdictDecision(decision).Known() {
			return &ParseError{Reason: fmt.Sprintf("kind=verdict: unknown decision %q (allowed: continue | amend | inquire | finish)", decision)}
		}
		if err := validateCheckerACUpdateWire(body); err != nil {
			return err
		}
	case KindResearch:
		body, _ := raw["body"].(map[string]any)
		if body == nil {
			return &ParseError{Reason: "kind=research requires a body object"}
		}
		// `done` is required so the runtime can decide whether to
		// re-fire the research role or move to the planner. Empty
		// `clarifications` + `done: false` is legitimate (the role
		// asks for one more turn to process prior_answers before
		// finalising findings).
		if _, ok := body["done"]; !ok {
			return &ParseError{Reason: "kind=research requires body.done (boolean)"}
		}
		if v, ok := body["done"].(bool); !ok {
			return &ParseError{Reason: fmt.Sprintf("kind=research: body.done must be a boolean, got %T", body["done"])}
		} else if !v {
			// done=false REQUIRES at least one clarification — the
			// only sensible reason to re-fire research is to ask
			// the user something. Without clarifications it would
			// loop forever.
			cl, _ := body["clarifications"].([]any)
			if len(cl) == 0 {
				return &ParseError{Reason: "kind=research: body.done=false requires at least one entry in body.clarifications"}
			}
		}
		// When done=true the role MUST emit `findings` (free-form
		// summary the planner reads). Empty findings on done=true
		// is a misbehaving role — the runtime fails loud so the
		// retry path engages.
		if done, _ := body["done"].(bool); done {
			findings, _ := body["findings"].(string)
			if strings.TrimSpace(findings) == "" {
				return &ParseError{Reason: "kind=research: body.findings is required when body.done=true (one-paragraph summary of what was learned)"}
			}
		}
		if cl, ok := body["clarifications"]; ok {
			arr, isArr := cl.([]any)
			if !isArr {
				return &ParseError{Reason: "kind=research: body.clarifications must be an array of objects"}
			}
			if len(arr) > researchMaxClarificationsPerBatch {
				return &ParseError{Reason: fmt.Sprintf("kind=research: body.clarifications has %d entries; max %d per batch", len(arr), researchMaxClarificationsPerBatch)}
			}
			seen := make(map[string]struct{}, len(arr))
			for i, e := range arr {
				entry, ok := e.(map[string]any)
				if !ok {
					return &ParseError{Reason: fmt.Sprintf("kind=research: body.clarifications[%d] must be an object", i)}
				}
				q, _ := entry["question"].(string)
				if strings.TrimSpace(q) == "" {
					return &ParseError{Reason: fmt.Sprintf("kind=research: body.clarifications[%d].question is required", i)}
				}
				id, _ := entry["id"].(string)
				if id == "" {
					// Soft recovery — auto-assign q1/q2/... isn't done
					// here (we'd mutate raw); the runtime fills it in
					// after parsing. We only reject duplicates that
					// the role itself emitted.
					continue
				}
				if _, dup := seen[id]; dup {
					return &ParseError{Reason: fmt.Sprintf("kind=research: body.clarifications[%d].id = %q duplicates an earlier entry", i, id)}
				}
				seen[id] = struct{}{}
				if k, _ := entry["kind"].(string); k != "" {
					switch k {
					case "required", "optional", "comment":
					default:
						return &ParseError{Reason: fmt.Sprintf("kind=research: body.clarifications[%d].kind = %q: must be one of [required, optional, comment]", i, k)}
					}
				}
			}
		}
	}
	return nil
}

// researchMaxClarificationsPerBatch caps the number of questions
// one research output can emit. Mirrors spec §2.7 cap (default 8).
// Above the cap the runtime treats it as a contract violation and
// fails the parse — the retry path then prompts the role to
// narrow its batch.
const researchMaxClarificationsPerBatch = 8

// VerdictDecision is the typed enum the checker emits to direct
// the planner loop. Phase C — four-valued: continue, amend,
// inquire, finish. Unknown values are rejected at validateRequired
// time so the loop only ever branches on a known shape.
type VerdictDecision string

const (
	VerdictContinue VerdictDecision = "continue"
	VerdictAmend    VerdictDecision = "amend"
	VerdictInquire  VerdictDecision = "inquire"
	VerdictFinish   VerdictDecision = "finish"
)

// Known reports whether v is one of the four declared decisions.
func (v VerdictDecision) Known() bool {
	switch v {
	case VerdictContinue, VerdictAmend, VerdictInquire, VerdictFinish:
		return true
	default:
		return false
	}
}

// DecodeVerdict re-marshals a parsed kind=verdict body into the
// typed Verdict AST. Pre-condition: h.Kind == KindVerdict and
// ParseHandoff succeeded (so validateRequired passed). Returns
// (Verdict, nil) on success.
func DecodeVerdict(h Handoff) (Verdict, error) {
	if h.Kind != KindVerdict {
		return Verdict{}, fmt.Errorf("mission: DecodeVerdict: handoff kind=%q, want verdict", h.Kind)
	}
	body, ok := h.Body.(map[string]any)
	if !ok {
		return Verdict{}, fmt.Errorf("mission: DecodeVerdict: body is not an object (got %T)", h.Body)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Verdict{}, fmt.Errorf("mission: DecodeVerdict: marshal body: %w", err)
	}
	var v Verdict
	if err := json.Unmarshal(raw, &v); err != nil {
		return Verdict{}, fmt.Errorf("mission: DecodeVerdict: unmarshal: %w", err)
	}
	if !v.Decision.Known() {
		return Verdict{}, fmt.Errorf("mission: DecodeVerdict: decision %q is unknown", v.Decision)
	}
	return v, nil
}

// DecodePlan re-marshals a parsed kind=plan body into the typed
// Plan AST the executor consumes. Pre-condition: h.Kind == KindPlan
// and ParseHandoff succeeded (so validateRequired passed). Returns
// (nil, nil) when next_wave was JSON-null — the planner's
// "plan_complete" signal. Returns (Plan, nil) otherwise with
// NextWave fully populated.
//
// Decoding goes through encoding/json (the parser already kept the
// body as a generic map; DecodePlan re-marshals + strict-unmarshals
// into Plan so unknown fields land in the struct's tag-matched
// slots and unrecognised fields are silently dropped — Phase I
// tightens this with output_contract.schema).
func DecodePlan(h Handoff) (*Plan, error) {
	if h.Kind != KindPlan {
		return nil, fmt.Errorf("mission: DecodePlan: handoff kind=%q, want plan", h.Kind)
	}
	body, ok := h.Body.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("mission: DecodePlan: body is not an object (got %T)", h.Body)
	}
	if body["next_wave"] == nil {
		return nil, nil
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("mission: DecodePlan: marshal body: %w", err)
	}
	var p Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("mission: DecodePlan: unmarshal: %w", err)
	}
	if p.NextWave.Label == "" {
		return nil, fmt.Errorf("mission: DecodePlan: next_wave.label is empty after decode")
	}
	if len(p.NextWave.Subagents) == 0 {
		return nil, fmt.Errorf("mission: DecodePlan: next_wave.subagents is empty after decode")
	}
	return &p, nil
}

// validateCheckerACUpdateWire shape-checks body.ac_update for kind=verdict
// emissions. Checker is status-tracking authority only — entries
// carrying `statement` / `drop` belong to the planner channel and
// must be rejected here (the runtime would refuse to apply them
// otherwise, but a wire-level error gives the checker a clearer
// retry signal). Phase 5.x — B11 §3.5.
func validateCheckerACUpdateWire(body map[string]any) error {
	updRaw, ok := body["ac_update"]
	if !ok || updRaw == nil {
		return nil
	}
	updList, ok := updRaw.([]any)
	if !ok {
		return &ParseError{Reason: "kind=verdict: body.ac_update must be an array (omit when empty)"}
	}
	for i, e := range updList {
		entry, ok := e.(map[string]any)
		if !ok {
			return &ParseError{Reason: fmt.Sprintf("kind=verdict: body.ac_update[%d] must be an object {id, status, evidence?}", i)}
		}
		id, _ := entry["id"].(string)
		if strings.TrimSpace(id) == "" {
			return &ParseError{Reason: fmt.Sprintf("kind=verdict: body.ac_update[%d].id is required (the row's stable ac-N id)", i)}
		}
		if _, hasStmt := entry["statement"]; hasStmt {
			return &ParseError{Reason: fmt.Sprintf("kind=verdict: body.ac_update[%d] cannot carry statement — checker cannot rewrite criteria; raise an issue in the verdict body instead", i)}
		}
		if drop, _ := entry["drop"].(bool); drop {
			return &ParseError{Reason: fmt.Sprintf("kind=verdict: body.ac_update[%d] cannot carry drop=true — checker cannot drop criteria; raise an issue in the verdict body instead", i)}
		}
		status, _ := entry["status"].(string)
		if strings.TrimSpace(status) == "" {
			return &ParseError{Reason: fmt.Sprintf("kind=verdict: body.ac_update[%d].status is required (unsatisfied|satisfied)", i)}
		}
		switch ACStatus(strings.TrimSpace(status)) {
		case ACUnsatisfied, ACSatisfied:
			// ok
		case ACDropped:
			return &ParseError{Reason: fmt.Sprintf("kind=verdict: body.ac_update[%d].status=\"dropped\" is not a valid wire value — checker cannot drop criteria", i)}
		default:
			return &ParseError{Reason: fmt.Sprintf("kind=verdict: body.ac_update[%d].status=%q is not one of unsatisfied|satisfied", i, status)}
		}
	}
	return nil
}

// validatePlanACDiffWire shape-checks body.ac_add / body.ac_update
// against the §3.2 wire schema. Both fields are optional at this
// layer — the planner loop enforces "≥1 AC must exist after iter 1
// applies its diff" because that check needs state context. Here we
// only verify what's present is well-formed.
func validatePlanACDiffWire(body map[string]any) error {
	if addRaw, ok := body["ac_add"]; ok && addRaw != nil {
		addList, ok := addRaw.([]any)
		if !ok {
			return &ParseError{Reason: "kind=plan: body.ac_add must be an array (omit when empty)"}
		}
		for i, e := range addList {
			entry, ok := e.(map[string]any)
			if !ok {
				return &ParseError{Reason: fmt.Sprintf("kind=plan: body.ac_add[%d] must be an object {statement, origin?}", i)}
			}
			stmt, _ := entry["statement"].(string)
			if strings.TrimSpace(stmt) == "" {
				return &ParseError{Reason: fmt.Sprintf("kind=plan: body.ac_add[%d].statement is required (non-empty string)", i)}
			}
		}
	}
	if updRaw, ok := body["ac_update"]; ok && updRaw != nil {
		updList, ok := updRaw.([]any)
		if !ok {
			return &ParseError{Reason: "kind=plan: body.ac_update must be an array (omit when empty)"}
		}
		for i, e := range updList {
			entry, ok := e.(map[string]any)
			if !ok {
				return &ParseError{Reason: fmt.Sprintf("kind=plan: body.ac_update[%d] must be an object {id, statement?, drop?, status?, evidence?}", i)}
			}
			id, _ := entry["id"].(string)
			if strings.TrimSpace(id) == "" {
				return &ParseError{Reason: fmt.Sprintf("kind=plan: body.ac_update[%d].id is required (e.g. \"ac-1\")", i)}
			}
			stmt, hasStmt := entry["statement"].(string)
			hasStmtField := hasStmt && strings.TrimSpace(stmt) != ""
			drop, _ := entry["drop"].(bool)
			status, hasStatus := entry["status"].(string)
			hasStatusField := hasStatus && strings.TrimSpace(status) != ""
			if !hasStmtField && !drop && !hasStatusField {
				return &ParseError{Reason: fmt.Sprintf("kind=plan: body.ac_update[%d] must carry at least one of statement / drop / status", i)}
			}
			if drop {
				reason, _ := entry["drop_reason"].(string)
				if strings.TrimSpace(reason) == "" {
					return &ParseError{Reason: fmt.Sprintf("kind=plan: body.ac_update[%d].drop=true requires drop_reason (one short sentence)", i)}
				}
			}
			if hasStatusField {
				switch ACStatus(strings.TrimSpace(status)) {
				case ACUnsatisfied, ACSatisfied:
					// ok
				case ACDropped:
					return &ParseError{Reason: fmt.Sprintf("kind=plan: body.ac_update[%d].status=\"dropped\" is not a wire value — set drop=true with drop_reason instead", i)}
				default:
					return &ParseError{Reason: fmt.Sprintf("kind=plan: body.ac_update[%d].status=%q is not one of unsatisfied|satisfied", i, status)}
				}
			}
		}
	}
	return nil
}

func truncateBody(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
