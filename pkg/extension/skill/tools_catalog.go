package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
	skillpkg "github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// `skill:tools_catalog` is the tool-discovery surface the LLM
// reaches for when it needs to see what providers/tools the agent
// has registered. It lives on the skill extension because the
// fields the model cares about — `granted_to_session`,
// `available_in_skills` — are pure skill-bindings projections.
// Naming-wise the tool isn't strictly skill-specific (it shows
// every registered provider, not just skill-related ones); the
// trade-off is one less package boundary while we have a single
// home for skill-related discovery.
//
// The handler:
//
//   - reads the unfiltered Snapshot off [SessionState.Tools()];
//   - applies the calling session's allow-set
//     (allowedFromBindings) to compute granted_to_session;
//   - walks all skills in the agent's store and builds an index
//     {fullToolName -> []skillName} so each entry carries
//     `available_in_skills` (the set of skills whose
//     allowed-tools admit this tool, including ones not loaded);
//   - applies the optional `provider` exact filter and `pattern`
//     case-insensitive substring filter.

const (
	toolNameToolsCatalog       = providerName + ":tools_catalog"
	permObjectToolsCatalog     = "hugen:tool:system"
	toolDescToolsCatalog       = "Returns the catalogue of every provider and tool the agent process has registered. `granted_to_session` reflects whether the calling session's loaded skills admit each tool. `available_in_skills` lists every skill in the catalogue whose allowed-tools admit it (use one of those names with `skill:load` to enable the tool when `granted_to_session=false`). Optional filters: `provider` (exact name) + `pattern` (case-insensitive substring on tool name)."
	toolsCatalogSchema         = `{
  "type": "object",
  "properties": {
    "provider": {"type": "string", "description": "Optional provider name filter (e.g. \"hugr-main\"). Returns all providers when omitted."},
    "pattern":  {"type": "string", "description": "Optional case-insensitive substring filter on tool name."}
  }
}`
)

type toolsCatalogInput struct {
	Provider string `json:"provider,omitempty"`
	Pattern  string `json:"pattern,omitempty"`
}

type toolsCatalogEntry struct {
	Name              string   `json:"name"`
	Description       string   `json:"description,omitempty"`
	PermissionObject  string   `json:"permission_object,omitempty"`
	GrantedToSession  bool     `json:"granted_to_session"`
	AvailableInSkills []string `json:"available_in_skills,omitempty"`
}

type toolsCatalogProvider struct {
	Name     string              `json:"name"`
	Lifetime string              `json:"lifetime,omitempty"`
	Tools    []toolsCatalogEntry `json:"tools"`
}

type toolsCatalogResult struct {
	Providers []toolsCatalogProvider `json:"providers"`
}

func (h *SessionSkill) callToolsCatalog(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	state, ok := extension.SessionStateFromContext(ctx)
	if !ok || state == nil {
		return nil, fmt.Errorf("%w: skill:tools_catalog: no session attached to dispatch ctx", tool.ErrSystemUnavailable)
	}
	tm := state.Tools()
	if tm == nil {
		return nil, tool.ErrSystemUnavailable
	}
	var in toolsCatalogInput
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("%w: skill:tools_catalog: %v", tool.ErrArgValidation, err)
		}
	}
	pattern := strings.ToLower(strings.TrimSpace(in.Pattern))
	wantProvider := strings.TrimSpace(in.Provider)

	snap, err := tm.Snapshot(ctx, h.sessionID)
	if err != nil {
		return nil, fmt.Errorf("skill:tools_catalog: snapshot: %w", err)
	}

	// granted_to_session: tools the loaded skills admit (the same
	// allow-set FilterTools applies to the model-facing snapshot).
	allowed := allowedFromHandle(ctx, h)
	skillIndex := buildSkillToolsIndex(ctx, h.manager)

	groups := make(map[string][]toolsCatalogEntry)
	for _, t := range snap.Tools {
		if wantProvider != "" && t.Provider != wantProvider {
			continue
		}
		if pattern != "" && !strings.Contains(strings.ToLower(t.Name), pattern) {
			continue
		}
		granted := true
		if allowed != nil {
			granted = allowed.match(t.Name)
		}
		groups[t.Provider] = append(groups[t.Provider], toolsCatalogEntry{
			Name:              t.Name,
			Description:       t.Description,
			PermissionObject:  t.PermissionObject,
			GrantedToSession:  granted,
			AvailableInSkills: skillIndex.matching(t.Name),
		})
	}

	out := toolsCatalogResult{Providers: make([]toolsCatalogProvider, 0, len(groups))}
	for name, tools := range groups {
		sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
		entry := toolsCatalogProvider{Name: name, Tools: tools}
		if lt, ok := tm.ProviderLifetime(name); ok {
			entry.Lifetime = lt.String()
		}
		out.Providers = append(out.Providers, entry)
	}
	sort.Slice(out.Providers, func(i, j int) bool { return out.Providers[i].Name < out.Providers[j].Name })
	return json.Marshal(out)
}

// skillToolsIndex is the fully-qualified-tool-name → []skill-name
// projection of every skill manifest in the store. Cheap to build
// per call (skills count × allowed-tools entries) and bounded —
// catalogue listings happen on user discovery, not in the model
// hot path.
type skillToolsIndex struct {
	exact    map[string][]string
	patterns []skillToolPattern
}

type skillToolPattern struct {
	prefix string
	skill  string
}

func buildSkillToolsIndex(ctx context.Context, sm *skillpkg.SkillManager) *skillToolsIndex {
	idx := &skillToolsIndex{exact: map[string][]string{}}
	if sm == nil {
		return idx
	}
	all, err := sm.List(ctx)
	if err != nil {
		return idx
	}
	for _, sk := range all {
		for _, g := range sk.Manifest.AllowedTools {
			for _, t := range g.Tools {
				full := g.Provider + ":" + t
				if strings.HasSuffix(t, "*") {
					idx.patterns = append(idx.patterns, skillToolPattern{
						prefix: strings.TrimSuffix(full, "*"),
						skill:  sk.Manifest.Name,
					})
					continue
				}
				idx.exact[full] = append(idx.exact[full], sk.Manifest.Name)
			}
		}
	}
	return idx
}

// matching returns the skill names whose allowed-tools admit the
// fully-qualified tool name (e.g. "hugr-main:discovery-list"),
// in stable alphabetical order, deduped.
func (idx *skillToolsIndex) matching(name string) []string {
	if idx == nil {
		return nil
	}
	set := map[string]struct{}{}
	for _, s := range idx.exact[name] {
		set[s] = struct{}{}
	}
	for _, p := range idx.patterns {
		if strings.HasPrefix(name, p.prefix) {
			set[p.skill] = struct{}{}
		}
	}
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
