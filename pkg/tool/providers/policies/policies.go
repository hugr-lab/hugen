package policies

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/store/queries"
	"github.com/hugr-lab/hugen/pkg/tool"
	"github.com/hugr-lab/query-engine/types"
)

// Policies is the Tier-3 store façade plus the LLM-facing
// `policy:save` / `policy:revoke` provider. One instance per
// agent, registered with ToolManager via both SetPolicies (to
// drive the Resolve consultation) and AddProvider (to expose
// the tools to the model).
//
// The struct owns the querier directly — phase 4.1c retired the
// legacy `tool.Policies` wrapper. Pass q=nil to construct a
// disabled instance (IsConfigured reports false; tool calls
// surface ErrSystemUnavailable). perms gates the
// hugen:policy:persist permission consulted by callSave /
// callRevoke; nil disables the gate (tests). log captures
// failed-write events; nil falls back to a discard handler.
type Policies struct {
	q     types.Querier
	perms perm.Service
	log   *slog.Logger
}

// New constructs the Tier-3 façade. q is the GraphQL querier
// against the local store (nil disables Tier-3); perms is the
// permission service consulted for hugen:policy:persist; log is
// optional.
func New(q types.Querier, perms perm.Service, log *slog.Logger) *Policies {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Policies{q: q, perms: perms, log: log}
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
func (p *Policies) Save(ctx context.Context, in Input) (string, error) {
	if !p.IsConfigured() {
		return "", tool.ErrSystemUnavailable
	}
	if in.AgentID == "" {
		return "", errors.New("policy: save: agent_id required")
	}
	if in.ToolName == "" {
		return "", errors.New("policy: save: tool_name required")
	}
	if in.Scope == "" {
		in.Scope = ScopeGlobal
	}
	if in.CreatedBy == "" {
		in.CreatedBy = CreatorUser
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
		return tool.ErrSystemUnavailable
	}
	agentID, toolName, scope, err := ParsePolicyID(id)
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

// Decide consults the persisted rows for `agentID`, evaluating
// the most-specific match against `toolName` honouring `*`-suffix
// globs. Returns a zero PolicyDecision (Outcome == PolicyAsk)
// when no row matches.
//
// Resolution chain: role exact → role prefix → skill exact →
// skill prefix → global exact → global prefix. Pass scope=""
// (or ScopeGlobal) to consult global rules only; pass
// "skill:<name>" or "role:<skill>:<role>" for the more specific
// scopes — Decide walks downwards from the supplied scope to
// global automatically.
func (p *Policies) Decide(ctx context.Context, agentID, toolName, scope string) (tool.PolicyDecision, error) {
	if !p.IsConfigured() {
		return tool.PolicyDecision{}, nil
	}
	if agentID == "" || toolName == "" {
		return tool.PolicyDecision{}, nil
	}
	rows, err := p.list(ctx, agentID)
	if err != nil {
		return tool.PolicyDecision{}, err
	}
	if len(rows) == 0 {
		return tool.PolicyDecision{}, nil
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
					return tool.PolicyDecision{}, err
				}
				return tool.PolicyDecision{
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
				return tool.PolicyDecision{}, err
			}
			return tool.PolicyDecision{
				Outcome:  out,
				ToolName: r.ToolName,
				Scope:    r.Scope,
				Reason:   reasonOf(r),
			}, nil
		}
	}
	return tool.PolicyDecision{}, nil
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

func (p *Policies) insert(ctx context.Context, in Input) error {
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

func (p *Policies) update(ctx context.Context, in Input) (int, error) {
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

// ensure Policies satisfies the contracts declared in pkg/tool.
var (
	_ tool.PolicyService = (*Policies)(nil)
	_ tool.ToolProvider  = (*Policies)(nil)
)
