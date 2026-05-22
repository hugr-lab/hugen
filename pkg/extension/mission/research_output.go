package mission

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ResearchOutput is the typed projection of a kind=research
// handoff body. Phase 5.x — B15. Roles emit one of two shapes:
//
//   - `done: false` with Clarifications populated — the runtime
//     batches the clarifications into a single user-facing
//     `session:inquire` modal, collects answers, and re-fires the
//     research role with PriorAnswers / PriorComments folded into
//     its context. Re-fires capped by manifest's MaxIterations.
//   - `done: true` with Findings + (optional) ResolvedUserInputs
//     + (optional) ACProposals — the runtime stamps these on
//     MissionState and proceeds to spawn the planner. The
//     planner sees Findings under [Plan context] /
//     plan_context.research_findings.
//
// MemorySummary is carried into PlanContext as the research
// stage's `phase=research` entry — short summary the planner sees
// alongside Findings + ResolvedUserInputs.
type ResearchOutput struct {
	Clarifications     []ResearchClarification `json:"clarifications,omitempty"`
	ResolvedUserInputs map[string]any          `json:"resolved_user_inputs,omitempty"`
	Done               bool                    `json:"done"`
	Findings           string                  `json:"findings,omitempty"`
	ACProposals        []ResearchACProposal    `json:"ac_proposals,omitempty"`
	MemorySummary      string                  `json:"memory_summary,omitempty"`
}

// ResearchClarification mirrors protocol.Clarification on the
// research-output side. The runtime hands this through to
// session:inquire verbatim once parsed.
type ResearchClarification struct {
	ID           string   `json:"id,omitempty"`
	Question     string   `json:"question"`
	Kind         string   `json:"kind,omitempty"`
	Options      []string `json:"options,omitempty"`
	Default      string   `json:"default,omitempty"`
	AllowComment *bool    `json:"allow_comment,omitempty"`
}

// ResearchACProposal is one proposed acceptance criterion the
// research role surfaces for the planner to consider. The planner
// is the authority (§3.2.1) — proposals are input, not commitment.
// Rationale + OriginClarification let the planner trace WHY the
// criterion was proposed when it composes the final AC set.
type ResearchACProposal struct {
	Statement           string `json:"statement"`
	Rationale           string `json:"rationale,omitempty"`
	OriginClarification string `json:"origin_clarification,omitempty"`
}

// DecodeResearchOutput re-marshals a parsed kind=research body
// into the typed ResearchOutput. Pre-condition: h.Kind ==
// KindResearch and ParseHandoff succeeded (so validateRequired
// passed the basic shape check). Returns an error when the body
// type is unexpected or the decode fails — both are runtime bugs
// (parser should have rejected them), but the function fails
// loud so the caller can route to the retry path.
//
// Soft recovery: clarifications missing an `id` get auto-assigned
// `q1`, `q2`, ... in order. Logged warning is the caller's
// responsibility — this function returns the typed value with
// ids filled in.
func DecodeResearchOutput(h Handoff) (*ResearchOutput, error) {
	if h.Kind != KindResearch {
		return nil, fmt.Errorf("mission: DecodeResearchOutput: handoff kind=%q, want research", h.Kind)
	}
	body, ok := h.Body.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("mission: DecodeResearchOutput: body is not an object (got %T)", h.Body)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("mission: DecodeResearchOutput: marshal body: %w", err)
	}
	var out ResearchOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("mission: DecodeResearchOutput: unmarshal: %w", err)
	}
	// Soft recovery — auto-assign ids to clarifications the role
	// didn't id. Skipping this would force the runtime's batched
	// inquire to invent ids itself, which the role wouldn't be
	// able to reference on the next turn.
	for i := range out.Clarifications {
		if strings.TrimSpace(out.Clarifications[i].ID) == "" {
			out.Clarifications[i].ID = fmt.Sprintf("q%d", i+1)
		}
		// Default kind to required so missing kind doesn't slip
		// through as the empty string (which downstream treats
		// permissively).
		if out.Clarifications[i].Kind == "" {
			out.Clarifications[i].Kind = "required"
		}
	}
	return &out, nil
}
