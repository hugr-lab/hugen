package policies

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// providerName is the short name every tool exposed here carries
// as its prefix. Phase 4.1a strict-renames the legacy
// system:policy_save / system:policy_revoke to policy:save /
// policy:revoke; PermissionObject stays at hugen:policy:save /
// hugen:policy:revoke.
const providerName = "policy"

// ErrSystemUnavailable mirrors pkg/tool's sentinel — used when a
// tool call lands on an unconfigured Policies (no store wired).
var ErrSystemUnavailable = errors.New("policy: store not configured")

// Name implements tool.ToolProvider. Matches the prefix of every
// Tool the provider exposes.
func (p *Policies) Name() string { return providerName }

// Lifetime classifies the provider as agent-scoped — a single
// instance per agent persists across sessions.
func (p *Policies) Lifetime() tool.Lifetime { return tool.LifetimePerAgent }

// Subscribe is a no-op — Policies has no event stream.
func (p *Policies) Subscribe(ctx context.Context) (<-chan tool.ProviderEvent, error) {
	return nil, nil
}

// Close is a no-op — the underlying Store is owned by pkg/runtime.
func (p *Policies) Close() error { return nil }

// List returns the two Tier-3 policy tools.
func (p *Policies) List(ctx context.Context) ([]tool.Tool, error) {
	return []tool.Tool{
		{
			Name:             "policy:save",
			Description:      "Persist a Tier-3 personal policy for a tool. Use this when the user says \"always allow\" / \"always deny\" a tool. The policy never overrides operator floor (Tier 1) or role rules (Tier 2). Returns the composite id required by policy:revoke.",
			Provider:         providerName,
			PermissionObject: "hugen:policy:save",
			ArgSchema:        schemaSave,
		},
		{
			Name:             "policy:revoke",
			Description:      "Delete a Tier-3 personal policy by composite id (returned from policy:save). Idempotent.",
			Provider:         providerName,
			PermissionObject: "hugen:policy:revoke",
			ArgSchema:        schemaRevoke,
		},
	}, nil
}

// Call dispatches policy:save / policy:revoke. Args have already
// been validated by ToolManager.Resolve before reaching here.
func (p *Policies) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	switch name {
	case "save":
		return p.callSave(ctx, args)
	case "revoke":
		return p.callRevoke(ctx, args)
	default:
		return nil, fmt.Errorf("%w: %s", tool.ErrUnknownTool, name)
	}
}

func (p *Policies) callSave(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if !p.IsConfigured() {
		return nil, ErrSystemUnavailable
	}
	var in struct {
		ToolName string `json:"tool_name"`
		Decision string `json:"decision"`
		Scope    string `json:"scope,omitempty"`
		Note     string `json:"note,omitempty"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("%w: policy:save: %v", tool.ErrArgValidation, err)
	}
	if in.ToolName == "" {
		return nil, fmt.Errorf("%w: policy:save: tool_name required", tool.ErrArgValidation)
	}
	out, err := decodeDecision(in.Decision)
	if err != nil {
		return nil, fmt.Errorf("%w: policy:save: %v", tool.ErrArgValidation, err)
	}
	if err := p.gatePersist(ctx, in.ToolName); err != nil {
		return nil, err
	}
	id, err := p.Save(ctx, Input{
		AgentID:   p.agentID(ctx),
		ToolName:  in.ToolName,
		Scope:     in.Scope,
		Decision:  out,
		Note:      in.Note,
		CreatedBy: CreatorLLM,
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{
		"id":        id,
		"tool_name": in.ToolName,
		"decision":  out.String(),
	})
}

func (p *Policies) callRevoke(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if !p.IsConfigured() {
		return nil, ErrSystemUnavailable
	}
	var in struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("%w: policy:revoke: %v", tool.ErrArgValidation, err)
	}
	if in.ID == "" {
		return nil, fmt.Errorf("%w: policy:revoke: id required", tool.ErrArgValidation)
	}
	// Mirror policy:save's gate: parse tool_name out of the id so
	// Tier-1/2 can forbid revoke of policies on sensitive tools
	// (otherwise an LLM with revoke access could just delete a
	// pinned deny). Composite id shape: agentID|toolName|scope.
	_, toolName, _, perr := ParsePolicyID(in.ID)
	if perr != nil {
		return nil, fmt.Errorf("%w: policy:revoke: %v", tool.ErrArgValidation, perr)
	}
	if err := p.gatePersist(ctx, toolName); err != nil {
		return nil, err
	}
	if err := p.Revoke(ctx, in.ID); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"revoked": in.ID})
}

// gatePersist consults Tier-1 / Tier-2 for the
// hugen:policy:persist permission with field=<tool_name>.
// Deployments may forbid persistent overrides on sensitive tools
// from config; this is the seam for that. nil perms == allow.
func (p *Policies) gatePersist(ctx context.Context, toolName string) error {
	if p.perms == nil {
		return nil
	}
	got, err := p.perms.Resolve(ctx, "hugen:policy:persist", toolName)
	if err != nil {
		return err
	}
	if got.Disabled {
		return fmt.Errorf("%w: policy:save denied for %q", tool.ErrPermissionDenied, toolName)
	}
	return nil
}

// agentID extracts the policy-row owner for the in-flight call.
// Reads perm.Service if it advertises an AgentID accessor;
// falls back to the SessionContext metadata "agent_id" key;
// finally to "" (callSave will surface a Save error from the
// store layer).
func (p *Policies) agentID(ctx context.Context) string {
	type agentIDer interface {
		AgentID() string
	}
	if a, ok := p.perms.(agentIDer); ok {
		if id := a.AgentID(); id != "" {
			return id
		}
	}
	if sc, ok := perm.SessionFromContext(ctx); ok && sc.SessionMetadata != nil {
		return sc.SessionMetadata["agent_id"]
	}
	return ""
}

func decodeDecision(s string) (tool.PolicyOutcome, error) {
	switch s {
	case "allow", "always_allowed":
		return tool.PolicyAllow, nil
	case "deny", "denied":
		return tool.PolicyDeny, nil
	case "ask", "manual_required", "":
		return tool.PolicyAsk, nil
	}
	return tool.PolicyAsk, fmt.Errorf("unknown decision %q", s)
}

var (
	schemaSave = json.RawMessage(`{
  "type": "object",
  "properties": {
    "tool_name": {"type": "string", "description": "Fully-qualified tool name <provider>:<field>; trailing * accepted as a glob (e.g. \"hugr-main:data-*\")."},
    "decision":  {"type": "string", "enum": ["allow", "deny", "ask"], "description": "allow → run without prompting; deny → block; ask → defer to default."},
    "scope":     {"type": "string", "description": "global (default) | skill:<name> | role:<skill>:<role>"},
    "note":      {"type": "string", "description": "Optional free-form annotation persisted with the row."}
  },
  "required": ["tool_name", "decision"]
}`)
	schemaRevoke = json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {"type": "string", "description": "Composite policy id (agent|tool_name|scope) returned by policy:save."}
  },
  "required": ["id"]
}`)
)

// ensure Policies satisfies tool.ToolProvider.
var _ tool.ToolProvider = (*Policies)(nil)
