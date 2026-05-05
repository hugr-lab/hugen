package policies

import (
	"fmt"
	"strings"

	"github.com/hugr-lab/hugen/pkg/tool"
)

// Input is the payload Save accepts. The composite key is
// (AgentID, ToolName, Scope); writing the same key again is an
// UPSERT — `policy`, `note`, `updated_at` advance.
//
// ToolName uses the fully-qualified `<provider>:<field>` form,
// with `*` accepted as a trailing glob (e.g. `hugr-main:data-*`).
// The provider half is mandatory so we never accidentally match
// across providers.
type Input struct {
	AgentID   string
	ToolName  string
	Scope     string
	Decision  tool.PolicyOutcome
	Note      string
	CreatedBy string
}

const (
	// ScopeGlobal is the default scope when the caller does not
	// specify one.
	ScopeGlobal = "global"
	// CreatorUser tags a row authored by the human user via the
	// chat surface ("always allow X").
	CreatorUser = "user"
	// CreatorLLM tags a row the agent persisted itself.
	CreatorLLM = "llm"
	// CreatorSystem tags rows installed by the runtime (e.g. floor
	// pre-population).
	CreatorSystem = "system"
)

// policyID encodes the composite primary key as a single string
// the LLM (and the persistence layer) can pass around. The
// separator is `|` because tool names use `:` for the provider
// split and scope strings use `:` internally too.
func policyID(agentID, toolName, scope string) string {
	if scope == "" {
		scope = ScopeGlobal
	}
	return agentID + "|" + toolName + "|" + scope
}

// ParsePolicyID splits the composite key produced by Save into
// (agentID, toolName, scope). Exported for callers that need to
// gate revoke operations on the row's tool_name without reaching
// into the row directly (today: callRevoke in this package).
func ParsePolicyID(id string) (agentID, toolName, scope string, err error) {
	parts := strings.SplitN(id, "|", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", fmt.Errorf("policy: malformed id %q (want agent|tool|scope)", id)
	}
	return parts[0], parts[1], parts[2], nil
}

// scopeChain returns the resolution sequence to walk from the
// supplied scope down to global. A "role:<skill>:<role>" scope
// also consults "skill:<skill>" and "global"; "skill:<skill>"
// consults global; "global" consults only itself.
func scopeChain(scope string) []string {
	scope = strings.TrimSpace(scope)
	if scope == "" || scope == ScopeGlobal {
		return []string{ScopeGlobal}
	}
	out := []string{scope}
	if strings.HasPrefix(scope, "role:") {
		// role:<skill>:<role> → skill:<skill> → global
		rest := strings.TrimPrefix(scope, "role:")
		if i := strings.Index(rest, ":"); i > 0 {
			out = append(out, "skill:"+rest[:i])
		}
	}
	out = append(out, ScopeGlobal)
	return out
}

// parsePolicyOutcome decodes the on-disk tool_policies.policy
// column into a tool.PolicyOutcome. Empty / unknown legacy
// values map to PolicyAsk so a malformed row never crashes
// Decide; truly unrecognised values surface an error so callers
// can surface a tool_error envelope.
func parsePolicyOutcome(s string) (tool.PolicyOutcome, error) {
	switch s {
	case "always_allowed":
		return tool.PolicyAllow, nil
	case "denied":
		return tool.PolicyDeny, nil
	case "manual_required", "":
		return tool.PolicyAsk, nil
	default:
		return tool.PolicyAsk, fmt.Errorf("policy: unknown policy %q", s)
	}
}
