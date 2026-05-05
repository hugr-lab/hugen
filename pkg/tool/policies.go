package tool

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hugr-lab/hugen/pkg/store/queries"
	"github.com/hugr-lab/query-engine/types"
)

// Tier-3 personal tool policies. One row per (agent_id, tool_name,
// scope) in the local-store `tool_policies` table (migration
// 0.0.5/02-tool-policies.sql).
//
// The tier never overrides Tier-1 or Tier-2 — ToolManager only
// consults Decide AFTER the upstream Resolve has returned a
// non-disabled Permission. Outcomes:
//
//   - PolicyAllow → run without prompting.
//   - PolicyDeny  → block; ToolManager surfaces ErrPermissionDenied
//     with tier=user.
//   - PolicyAsk   → no opinion; defer to the default policy
//     (phase-5 HITL approval lands here; phase-3 treats Ask as
//     allow because the floor already approved the call).
//
// Resolution chain inside Decide (from most specific to least):
//
//	role  exact > role  prefix
//	skill exact > skill prefix
//	global exact > global prefix
//
// Phase-3 callers pass `scope` directly; the manager defaults to
// "global". Skill/role-scoped policies are persisted but only
// observed when the caller asks for them.

// PolicyOutcome is the persisted decision enum stored in the
// tool_policies.policy column.
type PolicyOutcome int

const (
	PolicyAsk PolicyOutcome = iota
	PolicyAllow
	PolicyDeny
)

// String returns the on-disk representation expected by the
// tool_policies.policy column.
func (o PolicyOutcome) String() string {
	switch o {
	case PolicyAllow:
		return "always_allowed"
	case PolicyDeny:
		return "denied"
	default:
		return "manual_required"
	}
}

func parsePolicyOutcome(s string) (PolicyOutcome, error) {
	switch s {
	case "always_allowed":
		return PolicyAllow, nil
	case "denied":
		return PolicyDeny, nil
	case "manual_required", "":
		return PolicyAsk, nil
	default:
		return PolicyAsk, fmt.Errorf("tool: unknown policy %q", s)
	}
}

const (
	// PolicyScopeGlobal is the default scope when the caller does
	// not specify one.
	PolicyScopeGlobal = "global"
	// PolicyCreatorUser tags a row authored by the human user via
	// the chat surface ("always allow X").
	PolicyCreatorUser = "user"
	// PolicyCreatorLLM tags a row the agent persisted itself.
	PolicyCreatorLLM = "llm"
	// PolicyCreatorSystem tags rows installed by the runtime
	// (e.g. floor pre-population).
	PolicyCreatorSystem = "system"
)

// PolicyInput is the payload Save accepts. The composite key is
// (AgentID, ToolName, Scope); writing the same key again is an
// UPSERT — `policy`, `note`, `updated_at` advance.
//
// ToolName uses the fully-qualified `<provider>:<field>` form, with
// `*` accepted as a trailing glob (e.g. `hugr-main:data-*`). The
// provider half is mandatory so we never accidentally match across
// providers.
type PolicyInput struct {
	AgentID   string
	ToolName  string
	Scope     string
	Decision  PolicyOutcome
	Note      string
	CreatedBy string
}

// PolicyDecision is what Decide returns. Reason carries a short
// human-readable description of the matched row (used in audit
// frames and tool_error envelopes).
type PolicyDecision struct {
	Outcome  PolicyOutcome
	ToolName string
	Scope    string
	Reason   string
}

// Policies is the Tier-3 store façade.
type Policies struct {
	q types.Querier
}

// NewPolicies wires the store façade against a GraphQL querier
// (typically the local-store engine).
func NewPolicies(q types.Querier) *Policies {
	return &Policies{q: q}
}

// IsConfigured reports whether the store has a backing querier.
// nil-safe so the runtime can wire a *Policies that gracefully
// no-ops when the local store is absent (e.g. ephemeral tests).
func (p *Policies) IsConfigured() bool {
	return p != nil && p.q != nil
}

// Save UPSERTs a row keyed by (AgentID, ToolName, Scope). If the
// row exists the policy/note are advanced; otherwise it inserts.
// Returns the composite id used by Revoke.
func (p *Policies) Save(ctx context.Context, in PolicyInput) (string, error) {
	if !p.IsConfigured() {
		return "", ErrSystemUnavailable
	}
	if in.AgentID == "" {
		return "", errors.New("tool: policy save: agent_id required")
	}
	if in.ToolName == "" {
		return "", errors.New("tool: policy save: tool_name required")
	}
	if in.Scope == "" {
		in.Scope = PolicyScopeGlobal
	}
	if in.CreatedBy == "" {
		in.CreatedBy = PolicyCreatorUser
	}
	id := policyID(in.AgentID, in.ToolName, in.Scope)
	updated, err := p.update(ctx, in)
	if err != nil {
		return "", err
	}
	if updated > 0 {
		return id, nil
	}
	if err := p.insert(ctx, in); err != nil {
		return "", err
	}
	return id, nil
}

// Revoke deletes the row with the supplied composite id (as
// produced by Save). Idempotent — missing rows return nil.
func (p *Policies) Revoke(ctx context.Context, id string) error {
	if !p.IsConfigured() {
		return ErrSystemUnavailable
	}
	agentID, toolName, scope, err := parsePolicyID(id)
	if err != nil {
		return err
	}
	return queries.RunMutation(ctx, p.q,
		`mutation ($agent: String!, $tool: String!, $scope: String!) {
			hub { db { agent {
				delete_tool_policies(filter: {
					agent_id: {eq: $agent},
					tool_name: {eq: $tool},
					scope: {eq: $scope}
				}) { affected_rows }
			}}}
		}`,
		map[string]any{"agent": agentID, "tool": toolName, "scope": scope},
	)
}

// Decide consults the persisted rows for `agentID`, evaluating the
// most-specific match against `toolName` honouring `*`-suffix
// globs. Returns PolicyAsk when no row matches.
//
// Resolution chain: role exact → role prefix → skill exact →
// skill prefix → global exact → global prefix. Pass scope=""
// (or PolicyScopeGlobal) to consult global rules only; pass
// "skill:<name>" or "role:<skill>:<role>" for the more specific
// scopes — Decide walks downwards from the supplied scope to
// global automatically.
func (p *Policies) Decide(ctx context.Context, agentID, toolName, scope string) (PolicyDecision, error) {
	if !p.IsConfigured() {
		return PolicyDecision{}, nil
	}
	if agentID == "" || toolName == "" {
		return PolicyDecision{}, nil
	}
	rows, err := p.list(ctx, agentID)
	if err != nil {
		return PolicyDecision{}, err
	}
	if len(rows) == 0 {
		return PolicyDecision{}, nil
	}
	scopes := scopeChain(scope)
	for _, sc := range scopes {
		// Exact match wins inside a scope before any prefix glob.
		for _, r := range rows {
			if r.Scope != sc {
				continue
			}
			if r.ToolName == toolName {
				out, err := parsePolicyOutcome(r.Policy)
				if err != nil {
					return PolicyDecision{}, err
				}
				return PolicyDecision{
					Outcome:  out,
					ToolName: r.ToolName,
					Scope:    r.Scope,
					Reason:   reasonOf(r),
				}, nil
			}
		}
		for _, r := range rows {
			if r.Scope != sc {
				continue
			}
			if !strings.HasSuffix(r.ToolName, "*") {
				continue
			}
			prefix := strings.TrimSuffix(r.ToolName, "*")
			if !strings.HasPrefix(toolName, prefix) {
				continue
			}
			out, err := parsePolicyOutcome(r.Policy)
			if err != nil {
				return PolicyDecision{}, err
			}
			return PolicyDecision{
				Outcome:  out,
				ToolName: r.ToolName,
				Scope:    r.Scope,
				Reason:   reasonOf(r),
			}, nil
		}
	}
	return PolicyDecision{}, nil
}

// policyRow is the projection of tool_policies pulled by `list`.
type policyRow struct {
	AgentID  string `json:"agent_id"`
	ToolName string `json:"tool_name"`
	Scope    string `json:"scope"`
	Policy   string `json:"policy"`
	Note     string `json:"note"`
}

func reasonOf(r policyRow) string {
	if r.Note != "" {
		return r.Note
	}
	return fmt.Sprintf("policy %s @ %s", r.Policy, r.Scope)
}

func (p *Policies) list(ctx context.Context, agentID string) ([]policyRow, error) {
	rows, err := queries.RunQuery[[]policyRow](ctx, p.q,
		`query ($agent: String!) {
			hub { db { agent {
				tool_policies(filter: {agent_id: {eq: $agent}}) {
					agent_id tool_name scope policy note
				}
			}}}
		}`,
		map[string]any{"agent": agentID},
		"hub.db.agent.tool_policies",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	return rows, nil
}

func (p *Policies) insert(ctx context.Context, in PolicyInput) error {
	data := map[string]any{
		"agent_id":   in.AgentID,
		"tool_name":  in.ToolName,
		"scope":      in.Scope,
		"policy":     in.Decision.String(),
		"created_by": in.CreatedBy,
	}
	if in.Note != "" {
		data["note"] = in.Note
	}
	return queries.RunMutation(ctx, p.q,
		`mutation ($data: hub_db_tool_policies_mut_input_data!) {
			hub { db { agent {
				insert_tool_policies(data: $data) { agent_id }
			}}}
		}`,
		map[string]any{"data": data},
	)
}

func (p *Policies) update(ctx context.Context, in PolicyInput) (int, error) {
	type res struct {
		AffectedRows int `json:"affected_rows"`
	}
	data := map[string]any{
		"policy": in.Decision.String(),
	}
	// note column is nullable; an empty note clears the previous
	// annotation deliberately.
	data["note"] = in.Note
	out, err := queries.RunQuery[res](ctx, p.q,
		`mutation ($agent: String!, $tool: String!, $scope: String!, $data: hub_db_tool_policies_mut_data!) {
			hub { db { agent {
				update_tool_policies(filter: {
					agent_id: {eq: $agent},
					tool_name: {eq: $tool},
					scope: {eq: $scope}
				}, data: $data) { affected_rows }
			}}}
		}`,
		map[string]any{
			"agent": in.AgentID,
			"tool":  in.ToolName,
			"scope": in.Scope,
			"data":  data,
		},
		"hub.db.agent.update_tool_policies",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return 0, nil
		}
		return 0, err
	}
	return out.AffectedRows, nil
}

// policyID encodes the composite primary key as a single string
// the LLM (and the persistence layer) can pass around. The
// separator is `|` because tool names use `:` for the provider
// split and scope strings use `:` internally too.
func policyID(agentID, toolName, scope string) string {
	if scope == "" {
		scope = PolicyScopeGlobal
	}
	return agentID + "|" + toolName + "|" + scope
}

func parsePolicyID(id string) (agentID, toolName, scope string, err error) {
	return ParsePolicyID(id)
}

// ParsePolicyID splits the composite key produced by policyID into
// (agentID, toolName, scope). Exported for subpackage callers
// (today: pkg/tool/providers/policies) that need to gate revoke
// operations on the row's tool_name without reaching into the
// row directly. Stage A step 7 retires the inner alias when the
// implementation moves into the subpackage.
func ParsePolicyID(id string) (agentID, toolName, scope string, err error) {
	parts := strings.SplitN(id, "|", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", fmt.Errorf("tool: malformed policy id %q (want agent|tool|scope)", id)
	}
	return parts[0], parts[1], parts[2], nil
}

// scopeChain returns the resolution sequence to walk from the
// supplied scope down to global. A "role:<skill>:<role>" scope
// also consults "skill:<skill>" and "global"; "skill:<skill>"
// consults global; "global" consults only itself.
func scopeChain(scope string) []string {
	scope = strings.TrimSpace(scope)
	if scope == "" || scope == PolicyScopeGlobal {
		return []string{PolicyScopeGlobal}
	}
	out := []string{scope}
	if strings.HasPrefix(scope, "role:") {
		// role:<skill>:<role> → skill:<skill> → global
		rest := strings.TrimPrefix(scope, "role:")
		if i := strings.Index(rest, ":"); i > 0 {
			out = append(out, "skill:"+rest[:i])
		}
	}
	out = append(out, PolicyScopeGlobal)
	return out
}
