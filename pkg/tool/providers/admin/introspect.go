package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// The introspection half of the `tool` provider. Where
// provider_add / provider_remove MUTATE the registry (admin-gated),
// `tool:providers` and `tool:tools` only READ it — the deterministic,
// non-semantic enumeration a skill author needs to name real
// `provider:tool` values in a task's `allowed_tools_default` instead
// of inventing them (the dominant dogfood failure). They carry a
// distinct, non-admin permission object so the authoring skill
// (`skill_builder`) can grant them without the admin tier.
//
// Resolution is hierarchy-driven: the handler reads the CALLING
// session's manager off the dispatch ctx and calls
// [tool.ToolManager.Catalogue], whose parent walk yields agent-level
// providers and whose local set yields session-level ones (the shell
// appears only when the session loaded it). The agent-root manager
// the provider is constructed with is the fallback when no session
// state rides the ctx (non-session callers / tests).

const (
	permObjectIntrospect = "hugen:tool:introspect"

	toolNameProviders = providerName + ":providers"
	toolNameTools     = providerName + ":tools"

	descProviders = "List every tool provider available to this session — agent-level providers plus any session-level ones currently loaded — with each provider's lifetime and tool count. Read this first to see which provider hosts the capability you need, then call `tool:tools` for its exact tool names. Plain enumeration (no search)."
	descTools     = "List the tools a provider exposes, with their EXACT `provider:tool` names. Use this to copy real tool names into a task's `allowed_tools_default` — never invent or compose tool names. `detailed: true` also returns each tool's argument schema; omit it for a lean name+summary listing. Optional `pattern` filters by case-insensitive substring on the tool name."

	schemaProviders = `{
  "type": "object",
  "properties": {
    "pattern": {"type": "string", "description": "Optional case-insensitive substring filter on the provider name."}
  }
}`
	schemaTools = `{
  "type": "object",
  "properties": {
    "provider": {"type": "string", "description": "Provider name (from tool:providers). Required."},
    "detailed": {"type": "boolean", "description": "When true, include each tool's argument schema. Default false (name + one-line summary only)."},
    "pattern":  {"type": "string", "description": "Optional case-insensitive substring filter on the tool name."}
  },
  "required": ["provider"]
}`
)

// introspectionTools returns the read-only registry-introspection
// tools appended to the admin provider's List.
func introspectionTools() []tool.Tool {
	return []tool.Tool{
		{
			Name:             toolNameProviders,
			Description:      descProviders,
			Provider:         providerName,
			PermissionObject: permObjectIntrospect,
			ArgSchema:        json.RawMessage(schemaProviders),
		},
		{
			Name:             toolNameTools,
			Description:      descTools,
			Provider:         providerName,
			PermissionObject: permObjectIntrospect,
			ArgSchema:        json.RawMessage(schemaTools),
		},
	}
}

// managerForCtx prefers the calling session's manager (full
// agent+session provider chain) off the dispatch ctx, falling back
// to the agent-root manager the provider was constructed with.
func (a *AdminProvider) managerForCtx(ctx context.Context) *tool.ToolManager {
	if state, ok := extension.SessionStateFromContext(ctx); ok && state != nil {
		if tm := state.Tools(); tm != nil {
			return tm
		}
	}
	return a.tools
}

type providerEntry struct {
	Name      string `json:"name"`
	Lifetime  string `json:"lifetime,omitempty"`
	ToolCount int    `json:"tool_count"`
}

func (a *AdminProvider) callProviders(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	var in struct {
		Pattern string `json:"pattern,omitempty"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("%w: tool:providers: %v", tool.ErrArgValidation, err)
		}
	}
	pattern := strings.ToLower(strings.TrimSpace(in.Pattern))

	mgr := a.managerForCtx(ctx)
	if mgr == nil {
		return nil, fmt.Errorf("%w: tool:providers: no tool manager on the dispatch context", tool.ErrSystemUnavailable)
	}
	cat, err := mgr.Catalogue(ctx)
	if err != nil {
		// Partial catalogue is still useful — a single provider's
		// List failure should not blank the whole listing. Log-worthy
		// for the caller but not fatal; return what resolved.
		_ = err
	}
	out := make([]providerEntry, 0, len(cat))
	for _, p := range cat {
		if pattern != "" && !strings.Contains(strings.ToLower(p.Name), pattern) {
			continue
		}
		out = append(out, providerEntry{
			Name:      p.Name,
			Lifetime:  p.Lifetime.String(),
			ToolCount: len(p.Tools),
		})
	}
	return json.Marshal(map[string]any{"providers": out})
}

type toolEntryBrief struct {
	Name    string `json:"name"`
	Summary string `json:"summary,omitempty"`
}

type toolEntryFull struct {
	Name             string          `json:"name"`
	Description      string          `json:"description,omitempty"`
	Arguments        json.RawMessage `json:"arguments,omitempty"`
	RequiresApproval bool            `json:"requires_approval,omitempty"`
}

func (a *AdminProvider) callTools(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	var in struct {
		Provider string `json:"provider"`
		Detailed bool   `json:"detailed,omitempty"`
		Pattern  string `json:"pattern,omitempty"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("%w: tool:tools: %v", tool.ErrArgValidation, err)
	}
	want := strings.TrimSpace(in.Provider)
	if want == "" {
		return nil, fmt.Errorf("%w: tool:tools: provider is required (see tool:providers)", tool.ErrArgValidation)
	}
	pattern := strings.ToLower(strings.TrimSpace(in.Pattern))

	mgr := a.managerForCtx(ctx)
	if mgr == nil {
		return nil, fmt.Errorf("%w: tool:tools: no tool manager on the dispatch context", tool.ErrSystemUnavailable)
	}
	cat, _ := mgr.Catalogue(ctx)
	var match *tool.ProviderCatalogue
	for i := range cat {
		if cat[i].Name == want {
			match = &cat[i]
			break
		}
	}
	if match == nil {
		return nil, fmt.Errorf("%w: tool:tools: no provider named %q — call tool:providers for the list", tool.ErrUnknownProvider, want)
	}

	if in.Detailed {
		full := make([]toolEntryFull, 0, len(match.Tools))
		for _, t := range match.Tools {
			if pattern != "" && !strings.Contains(strings.ToLower(t.Name), pattern) {
				continue
			}
			full = append(full, toolEntryFull{
				Name:             t.Name,
				Description:      t.Description,
				Arguments:        t.ArgSchema,
				RequiresApproval: t.RequiresApproval,
			})
		}
		return json.Marshal(map[string]any{"provider": want, "tools": full})
	}

	brief := make([]toolEntryBrief, 0, len(match.Tools))
	for _, t := range match.Tools {
		if pattern != "" && !strings.Contains(strings.ToLower(t.Name), pattern) {
			continue
		}
		brief = append(brief, toolEntryBrief{Name: t.Name, Summary: firstSentence(t.Description)})
	}
	return json.Marshal(map[string]any{"provider": want, "tools": brief})
}

// firstSentence trims a tool description to its first sentence (or a
// hard cap) for the brief listing.
func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '.'); i > 0 && i < 160 {
		return s[:i+1]
	}
	if len(s) > 160 {
		return strings.TrimSpace(s[:160]) + "…"
	}
	return s
}
