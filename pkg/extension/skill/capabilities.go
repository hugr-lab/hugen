package skill

import (
	"context"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// Phase-4.1b-pre stage 2: skill extension capabilities.
//
// Three capability methods land on *Extension:
//
//   - AdvertiseSystemPrompt — concatenates the bindings'
//     Instructions block (rendered manifests of every loaded skill)
//     with the catalogue of every skill the agent can reach.
//   - FilterTools — narrows the per-session tool catalogue to the
//     allow-list compiled from loaded skills' allowed-tools.
//   - Generation(state) — returns the SkillManager bindings
//     generation for the calling session so a load/unload bumps
//     the snapshot cache key.
//
// The extension reads SkillManager + sessionID via the per-session
// [*SessionSkill] handle stashed under [StateKey] at InitState
// (extension.go).

// Compile-time interface assertions.
var (
	_ extension.Advertiser         = (*Extension)(nil)
	_ extension.ToolFilter         = (*Extension)(nil)
	_ extension.GenerationProvider = (*Extension)(nil)
	_ extension.ToolPolicyAdvisor  = (*Extension)(nil)
	_ extension.SubagentDescriber  = (*Extension)(nil)
	_ extension.SubagentSpawnHinter = (*Extension)(nil)
)

// AdvertiseSystemPrompt implements [extension.Advertiser].
// Composes two sections: bindings.Instructions (the rendered body
// of every loaded skill — concrete tool-usage guidance) and the
// available-skills catalogue (one bullet per skill in the store
// with a `(loaded)` tag for skills already loaded into the session).
// Returns "" when nothing to render so the runtime skips the empty
// section.
func (e *Extension) AdvertiseSystemPrompt(ctx context.Context, state extension.SessionState) string {
	h := FromState(state)
	if h == nil || h.manager == nil {
		return ""
	}
	var parts []string
	if b, err := h.Bindings(ctx); err == nil && b.Instructions != "" {
		parts = append(parts, b.Instructions)
	}
	if cat := renderCatalogue(ctx, h); cat != "" {
		parts = append(parts, cat)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n")
}

// renderCatalogue produces the "## Available skills" section of
// the system prompt: one bullet per skill in the store using the
// manifest description. Loaded skills carry a `(loaded)` tag.
func renderCatalogue(ctx context.Context, h *SessionSkill) string {
	all, err := h.manager.List(ctx)
	if err != nil || len(all) == 0 {
		return ""
	}
	loadedSet := map[string]struct{}{}
	for _, n := range h.LoadedNames(ctx) {
		loadedSet[n] = struct{}{}
	}
	var b strings.Builder
	b.WriteString("## Available skills\n\nLoad any of these via the `skill:load` tool when their domain becomes relevant. Already-loaded skills are tagged `(loaded)`.\n\n")
	for _, sk := range all {
		b.WriteString("- `")
		b.WriteString(sk.Manifest.Name)
		b.WriteString("`")
		if _, on := loadedSet[sk.Manifest.Name]; on {
			b.WriteString(" (loaded)")
		}
		b.WriteString(" — ")
		b.WriteString(strings.TrimSpace(sk.Manifest.Description))
		b.WriteString("\n")
	}
	return b.String()
}

// FilterTools implements [extension.ToolFilter]. Narrows the
// catalogue to tools the loaded skills' allowed-tools admit.
//
// Legacy semantics from pkg/session/snapshot_cache.applySkillFilter
// preserved verbatim:
//   - allowedFromBindings returns nil when the SkillManager is nil
//     (no-skill deployment / tests) → no-op, return all.
//   - returns an empty (non-nil) set when no skill is loaded → the
//     catalogue collapses to empty (allowedSet.match returns false
//     for every name).
//   - populated set → returns matching tools.
func (e *Extension) FilterTools(ctx context.Context, state extension.SessionState, all []tool.Tool) []tool.Tool {
	h := FromState(state)
	if h == nil || h.manager == nil {
		return all
	}
	allowed := allowedFromHandle(ctx, h)
	if allowed == nil {
		return all
	}
	out := make([]tool.Tool, 0, len(all))
	for _, t := range all {
		if allowed.match(t.Name) {
			out = append(out, t)
		}
	}
	return out
}

// AdviseToolPolicy implements [extension.ToolPolicyAdvisor].
// Reads max_turns / max_turns_hard / stuck_detection from the
// loaded skills' bindings and reports them as a [ToolIterPolicy].
// Empty bindings (no skill loaded, or no SkillManager wired) yield
// the zero-valued policy, which the runtime treats as "no
// recommendation" and falls back to its own defaults.
func (e *Extension) AdviseToolPolicy(ctx context.Context, state extension.SessionState) extension.ToolIterPolicy {
	h := FromState(state)
	if h == nil || h.manager == nil {
		return extension.ToolIterPolicy{}
	}
	b, err := h.Bindings(ctx)
	if err != nil {
		return extension.ToolIterPolicy{}
	}
	return extension.ToolIterPolicy{
		SoftCap:            b.MaxTurns,
		HardCeiling:        b.MaxTurnsHard,
		DisableStuckNudges: b.StuckDetectionDisabled,
	}
}

// DescribeSubagent implements [extension.SubagentDescriber]. Walks
// every skill in the manager's catalog; on the first match returns
// SubagentValid (role empty or matches a declared subagent role)
// or SubagentSkillFoundRoleMissing. Returns SubagentUnknown when
// no skill in the catalog matches the requested name.
func (e *Extension) DescribeSubagent(ctx context.Context, state extension.SessionState, skillName, roleName string) (extension.SubagentValidation, error) {
	h := FromState(state)
	if h == nil || h.manager == nil {
		return extension.SubagentUnknown, nil
	}
	all, err := h.manager.List(ctx)
	if err != nil {
		return extension.SubagentUnknown, err
	}
	for _, s := range all {
		if s.Manifest.Name != skillName {
			continue
		}
		if roleName == "" {
			return extension.SubagentValid, nil
		}
		for _, r := range s.Manifest.Hugen.SubAgents {
			if r.Name == roleName {
				return extension.SubagentValid, nil
			}
		}
		return extension.SubagentSkillFoundRoleMissing, nil
	}
	return extension.SubagentUnknown, nil
}

// SubagentSpawnHint implements [extension.SubagentSpawnHinter]. Walks
// the manager's catalog for the requested (skill, role) and returns
// the role's manifest-declared Intent (empty string when not set or
// when the skill / role is unknown). Skill-level (no role) calls
// always return zero — only role authors can pin an intent.
func (e *Extension) SubagentSpawnHint(ctx context.Context, state extension.SessionState, skillName, roleName string) (extension.SubagentSpawnHint, error) {
	h := FromState(state)
	if h == nil || h.manager == nil || roleName == "" {
		return extension.SubagentSpawnHint{}, nil
	}
	all, err := h.manager.List(ctx)
	if err != nil {
		return extension.SubagentSpawnHint{}, err
	}
	for _, s := range all {
		if s.Manifest.Name != skillName {
			continue
		}
		for _, r := range s.Manifest.Hugen.SubAgents {
			if r.Name == roleName {
				return extension.SubagentSpawnHint{Intent: r.Intent}, nil
			}
		}
	}
	return extension.SubagentSpawnHint{}, nil
}

// Generation implements [extension.GenerationProvider]. Returns
// the SkillManager bindings generation for the calling session.
// Bumps on every Load / Unload / Publish that mutates the loaded
// set, invalidating the snapshot cache so a subsequent
// modelToolsForSession recomputes the allow-list.
func (e *Extension) Generation(state extension.SessionState) int64 {
	h := FromState(state)
	if h == nil {
		return 0
	}
	b, err := h.Bindings(context.Background())
	if err != nil {
		return 0
	}
	return b.Generation
}

// allowedSet is the per-session compiled allow-list. Holds both
// exact tool names and `provider:prefix*` glob patterns so a skill
// granting `discovery-*` against the `hugr-main` provider matches
// every `hugr-main:discovery-<anything>` tool.
//
// nil ⇒ no filter (the SkillManager is nil — tests / deployments
// without skill management). An empty (non-nil) set ⇒ no skill
// loaded → preserve legacy behaviour and return the catalogue
// unchanged (see FilterTools).
type allowedSet struct {
	exact    map[string]bool
	patterns []string
}

// match reports whether the fully-qualified tool name (e.g.
// "hugr-main:discovery-search_data_sources") is allowed by any
// rule in the set.
func (a *allowedSet) match(name string) bool {
	if a == nil {
		return true
	}
	if a.exact[name] {
		return true
	}
	for _, p := range a.patterns {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// allowedFromHandle compiles the loaded-skill bindings into an
// allowedSet. Returns nil when h is nil (no skill ext wired);
// an empty (non-nil) set when the session has no loaded skills;
// populated otherwise.
func allowedFromHandle(ctx context.Context, h *SessionSkill) *allowedSet {
	if h == nil {
		return nil
	}
	b, err := h.Bindings(ctx)
	if err != nil || len(b.AllowedTools) == 0 {
		return &allowedSet{exact: map[string]bool{}}
	}
	out := &allowedSet{exact: map[string]bool{}}
	for _, g := range b.AllowedTools {
		for _, t := range g.Tools {
			full := g.Provider + ":" + t
			if strings.HasSuffix(t, "*") {
				out.patterns = append(out.patterns, strings.TrimSuffix(full, "*"))
				continue
			}
			out.exact[full] = true
		}
	}
	return out
}

