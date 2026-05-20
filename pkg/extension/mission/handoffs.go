package mission

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Handoff is the structured artifact a worker / phase role emits at
// session-close. The Body field carries the worker's primary output
// (table, query, schema, prose answer); MemorySummary is a compact
// "what I learned" snippet auto-extracted into the Plan Context
// (phase D). Status reflects the worker's self-assessed termination
// state; Reason explains a non-ok status.
//
// Handoff is constructed by the output_contract parser from a
// YAML/JSON fenced block in the worker's terminal AgentMessage. The
// Kind field discriminates the shape: workers emit kind=handoff,
// planners kind=plan, checkers kind=verdict, synthesizers
// kind=synthesis.
type Handoff struct {
	// Ref is the handoff's lookup key in the Handoffs store. Format:
	// "<subagent_name>@<wave_label>". Assigned by the executor at
	// parse time; not authored by the worker.
	Ref string `json:"ref"`

	// Kind discriminates the contract shape. See OutputContractKind.
	Kind OutputContractKind `json:"kind"`

	// Status is the worker's self-assessed outcome: "ok" |
	// "partial" | "error". Required.
	Status string `json:"status"`

	// Reason explains a non-ok status. Required when Status != "ok".
	Reason string `json:"reason,omitempty"`

	// Body is the worker's primary output. Type depends on Kind:
	// strings for kind=handoff/synthesis, structured for kind=plan
	// (a Plan), kind=verdict (a Verdict).
	Body any `json:"body,omitempty"`

	// MemorySummary is a compact prose summary auto-extracted into
	// the Plan Context journal (phase D). 2-3 sentences max by
	// skill convention; runtime does not enforce length here.
	MemorySummary string `json:"memory_summary,omitempty"`

	// Subagent records who authored this handoff. SessionID is the
	// worker's session id; Name / Role / Skill mirror the spawn
	// metadata for catalog rendering.
	Subagent SubagentRef `json:"subagent"`

	// CreatedAt is the wall-clock time the executor recorded the
	// handoff (close-of-worker time, not turn-end time).
	CreatedAt time.Time `json:"created_at"`
}

// SubagentRef is the minimal author-info attached to every handoff.
// Lets catalog rendering / observability identify a ref's origin
// without joining back to session_events.
type SubagentRef struct {
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
	Role      string `json:"role,omitempty"`
	Skill     string `json:"skill,omitempty"`
}

// Verdict is the structured body for kind=verdict handoffs (Phase
// C). Decision drives runtime routing; Issues are passed to the
// next planner when Decision=amend; Reason mirrors the handoff's
// top-level reason so downstream code only needs to read one
// struct.
//
// Decoded from the kind=verdict body via [DecodeVerdict] after
// the output_contract parser validates the required fields.
type Verdict struct {
	// Decision is one of continue | amend | inquire | finish (see
	// [VerdictDecision] for the typed enum). The runtime routes
	// each: continue → next iteration, amend → replan with Issues
	// attached, inquire → checker calls session:inquire, finish →
	// emit plan_complete.
	Decision VerdictDecision `json:"decision"`

	// Issues lists concrete amendments the checker recommends.
	// Rendered into the next planner's first message as [Recent
	// verdict] when Decision=amend.
	Issues []string `json:"issues,omitempty"`

	// Reason is the checker's free-form justification — kept on
	// the typed AST so downstream consumers don't have to read
	// both Handoff.Reason and the kind=verdict body separately.
	Reason string `json:"reason,omitempty"`

	// Confidence is an optional 0-1 self-rating. Not load-bearing
	// in v1; recorded for telemetry.
	Confidence float64 `json:"confidence,omitempty"`

	// WaveACStatus lists per-criterion satisfaction for the
	// just-completed wave's acceptance_criteria. Empty when the
	// planner didn't set wave AC. Phase I.26.
	WaveACStatus []ACCriterionStatus `json:"wave_ac_status,omitempty"`

	// MissionACStatus lists per-criterion satisfaction for the
	// mission's acceptance_criteria as of this verdict. Runtime
	// gates `finish` on every entry being satisfied — a finish
	// verdict with any unsatisfied criterion is coerced to a
	// synthetic amend so the next planner can address the gap.
	// Phase I.26.
	MissionACStatus []ACCriterionStatus `json:"mission_ac_status,omitempty"`
}

// ACCriterionStatus is one row of the per-criterion check the
// checker emits. Phase I.26.
type ACCriterionStatus struct {
	Criterion string `json:"criterion"`
	Satisfied bool   `json:"satisfied"`
	Evidence  string `json:"evidence,omitempty"`
}

// Handoffs is the per-mission store: a flat map keyed by Ref. The
// store is append-once-overwrite-on-retry — a re-spawned worker
// with the same ref replaces the prior entry. Recovery from
// session_events on restart is a single replay pass.
//
// Handoffs is safe for concurrent reads + writes via its mu — the
// executor may parse handoffs on the ChildFrameObserver goroutine
// while a tool (mission:get_handoff) reads on a worker's dispatch
// goroutine.
type Handoffs struct {
	mu      sync.RWMutex
	entries map[string]Handoff
}

// NewHandoffs constructs an empty Handoffs store.
func NewHandoffs() *Handoffs {
	return &Handoffs{entries: make(map[string]Handoff)}
}

// Put records a handoff under its Ref, overwriting any prior entry
// with the same ref. The caller (executor) is responsible for
// assigning Ref before calling Put.
func (h *Handoffs) Put(handoff Handoff) {
	if handoff.Ref == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.entries == nil {
		h.entries = make(map[string]Handoff)
	}
	h.entries[handoff.Ref] = handoff
}

// Get returns the handoff for ref and ok=true when present, or the
// zero value and ok=false when missing.
func (h *Handoffs) Get(ref string) (Handoff, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	v, ok := h.entries[ref]
	return v, ok
}

// Len returns the number of handoffs currently in the store.
// Cheap concurrent-safe read for status projections.
func (h *Handoffs) Len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.entries)
}

// List returns every handoff in the store, ordered by CreatedAt
// ascending. Used by the [Available handoffs] catalog renderer.
func (h *Handoffs) List() []Handoff {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]Handoff, 0, len(h.entries))
	for _, v := range h.entries {
		out = append(out, v)
	}
	// Stable order: CreatedAt ascending, ties broken by Ref. Cheap
	// insertion sort — the store stays small (≤ tens of entries per
	// mission v1).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			a, b := out[j-1], out[j]
			if a.CreatedAt.Before(b.CreatedAt) {
				break
			}
			if a.CreatedAt.Equal(b.CreatedAt) && a.Ref < b.Ref {
				break
			}
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// MakeRef constructs the canonical "<subagent_name>@<wave_label>"
// ref. Empty inputs are rejected with an error so callers fail-fast
// rather than producing a malformed ref the parser would later
// reject.
func MakeRef(subagentName, waveLabel string) (string, error) {
	name := strings.TrimSpace(subagentName)
	wave := strings.TrimSpace(waveLabel)
	if name == "" {
		return "", fmt.Errorf("handoff ref: subagent name is empty")
	}
	if wave == "" {
		return "", fmt.Errorf("handoff ref: wave label is empty")
	}
	return name + "@" + wave, nil
}

// ParseRef splits a "<name>@<wave>" ref back into its parts.
// Returns an error if the ref is malformed.
func ParseRef(ref string) (name, wave string, err error) {
	parts := strings.SplitN(strings.TrimSpace(ref), "@", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("handoff ref: malformed %q (want \"<name>@<wave>\")", ref)
	}
	return parts[0], parts[1], nil
}
