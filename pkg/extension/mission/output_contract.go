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
)

// Known returns true when k is one of the four declared kinds.
func (k OutputContractKind) Known() bool {
	switch k {
	case KindHandoff, KindPlan, KindVerdict, KindSynthesis:
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
// `handoff` / `plan` / `verdict` / `synthesis`. Tolerant to
// surrounding prose — the parser scans for the first matching
// block; everything outside is treated as commentary.
//
// (?s) — dot matches newline.
var handoffBlockRe = regexp.MustCompile(
	"(?s)```(handoff|plan|verdict|synthesis)\\s*\\n(.+?)```",
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
// keys we care about. The full YAML dependency is a Phase B
// addition once we ship a planner; Phase A's experimental_inline
// fixture emits JSON only.
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

	if err := validateRequired(kind, h, generic); err != nil {
		return Handoff{}, err
	}
	return h, nil
}

// validateRequired enforces the per-kind required-field discipline.
// kind=handoff/synthesis: status required.
// kind=plan: status required + body must look like a Plan
//
//	(next_wave present).
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
			return &ParseError{Reason: "kind=plan requires a body object with next_wave"}
		}
		if _, ok := body["next_wave"]; !ok {
			return &ParseError{Reason: "kind=plan requires body.next_wave"}
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
	}
	return nil
}

func truncateBody(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
