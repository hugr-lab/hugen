package session

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/hugr-lab/hugen/pkg/tool"
)

// Step 21 of phase-4.1a-spec.md §9 — `session:tool_catalog` lands
// on the Manager with the per-session filtered view that phase 4.2
// originally scoped onto SystemProvider. Pre-empting it here keeps
// SystemProvider lean (post-step-20) and gives the LLM a discovery
// surface today.
//
// The handler:
//
//   - takes the unfiltered Snapshot from the caller's
//     *tool.ToolManager;
//   - tags each tool with `granted_to_session` from the session's
//     loaded-skill bindings (`allowedFromBindings` reused);
//   - groups by provider, attaches Lifetime via
//     ToolManager.ProviderLifetime;
//   - applies the optional `provider` exact filter and `pattern`
//     case-insensitive substring filter on tool name.

func init() {
	sessionTools["tool_catalog"] = sessionToolDescriptor{
		Name:             "tool_catalog",
		Description:      "Returns the catalogue of every provider and tool the agent process has registered. `granted_to_session` reflects whether the calling session's loaded skills admit each tool. Optional filters: `provider` (exact name) + `pattern` (case-insensitive substring on tool name).",
		PermissionObject: permObjectToolCatalog,
		ArgSchema:        json.RawMessage(toolCatalogSchema),
		Handler:          callToolCatalog,
	}
}

const permObjectToolCatalog = "hugen:session:tool_catalog"

const toolCatalogSchema = `{
  "type": "object",
  "properties": {
    "provider": {"type": "string", "description": "Optional provider name filter (e.g. \"hugr-main\"). Returns all providers when omitted."},
    "pattern":  {"type": "string", "description": "Optional case-insensitive substring filter on tool name."}
  }
}`

type toolCatalogInput struct {
	Provider string `json:"provider,omitempty"`
	Pattern  string `json:"pattern,omitempty"`
}

type toolCatalogEntry struct {
	Name             string `json:"name"`
	Description      string `json:"description,omitempty"`
	PermissionObject string `json:"permission_object,omitempty"`
	GrantedToSession bool   `json:"granted_to_session"`
}

type toolCatalogProvider struct {
	Name     string             `json:"name"`
	Lifetime string             `json:"lifetime,omitempty"`
	Tools    []toolCatalogEntry `json:"tools"`
}

type toolCatalogResult struct {
	Providers []toolCatalogProvider `json:"providers"`
}

func callToolCatalog(ctx context.Context, _ *Manager, args json.RawMessage) (json.RawMessage, error) {
	s, errFrame, err := callerSession(ctx)
	if errFrame != nil || err != nil {
		return errFrame, err
	}
	if s.tools == nil {
		return nil, tool.ErrSystemUnavailable
	}
	var in toolCatalogInput
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return toolErr("bad_request", fmt.Sprintf("invalid tool_catalog args: %v", err))
		}
	}
	pattern := strings.ToLower(strings.TrimSpace(in.Pattern))
	wantProvider := strings.TrimSpace(in.Provider)

	snap, err := s.tools.Snapshot(ctx, s.id)
	if err != nil {
		return toolErr("io", err.Error())
	}
	allowed := allowedFromBindings(ctx, s.skills, s.id)

	groups := make(map[string][]toolCatalogEntry)
	for _, t := range snap.Tools {
		if wantProvider != "" && t.Provider != wantProvider {
			continue
		}
		if pattern != "" && !strings.Contains(strings.ToLower(t.Name), pattern) {
			continue
		}
		groups[t.Provider] = append(groups[t.Provider], toolCatalogEntry{
			Name:             t.Name,
			Description:      t.Description,
			PermissionObject: t.PermissionObject,
			GrantedToSession: allowed.match(t.Name),
		})
	}

	out := toolCatalogResult{Providers: make([]toolCatalogProvider, 0, len(groups))}
	for name, tools := range groups {
		sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
		entry := toolCatalogProvider{Name: name, Tools: tools}
		if lt, ok := s.tools.ProviderLifetime(name); ok {
			entry.Lifetime = lt.String()
		}
		out.Providers = append(out.Providers, entry)
	}
	sort.Slice(out.Providers, func(i, j int) bool { return out.Providers[i].Name < out.Providers[j].Name })
	return json.Marshal(out)
}
