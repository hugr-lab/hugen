package mission

import (
	"encoding/json"
	"fmt"
)

// ResearchOutput is the typed projection of a kind=research
// handoff body. The research role runs ONCE before the planner and
// emits a single terminal result: the mission's open questions are
// already resolved (the role asked the user directly via
// session:inquire mid-turn when it hit an ambiguity), so the
// handoff carries only the distilled outcome.
//
//   - Findings — required. The concrete brief the planner reads:
//     what sources / tables / fields the mission hinges on (exact
//     names), which join keys were confirmed, and how each
//     ambiguity was resolved. Specific enough that downstream
//     workers lift names verbatim instead of re-discovering them.
//   - ResolvedUserInputs (optional) — the key/value answers the
//     role pulled from the user; the planner lifts them into
//     workers' `inputs`.
//   - ACProposals (optional) — acceptance criteria the role
//     suggests; the planner is the authority.
//   - MemorySummary (optional) — one line carried into PlanContext
//     as the research stage's `phase=research` entry.
//
// The runtime stamps Findings + ResolvedUserInputs + ACProposals
// on MissionState via SetResearchOutput and proceeds to the
// planner spawn — there is no clarification re-fire loop; the role
// owns its own HITL.
type ResearchOutput struct {
	ResolvedUserInputs map[string]any       `json:"resolved_user_inputs,omitempty"`
	Findings           string               `json:"findings,omitempty"`
	ACProposals        []ResearchACProposal `json:"ac_proposals,omitempty"`
	MemorySummary      string               `json:"memory_summary,omitempty"`
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
// confirmed body.findings is present). Returns an error when the
// body type is unexpected or the decode fails — both are runtime
// bugs (parser should have rejected them), but the function fails
// loud so the caller can route to the retry path.
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
	return &out, nil
}
