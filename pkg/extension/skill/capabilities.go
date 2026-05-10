package skill

import (
	"context"
	"io/fs"
	"sort"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
	skillpkg "github.com/hugr-lab/hugen/pkg/skill"
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
// Composes three sections in order:
//   - Loaded-skills metadata block: per-skill header with directory
//     and bundled-files listing (scripts/references/assets) so the
//     model can invoke artefacts via existing bash:run /
//     python:run_script with `${SKILL_DIR}/scripts/foo.py` etc.
//     Phase 4.2 §3.4. Skills with no on-disk Root (inline) emit only
//     name + description.
//   - bindings.Instructions: the rendered body of every loaded skill
//     (concrete tool-usage guidance).
//   - Available-skills catalogue: one bullet per skill in the store
//     with a `(loaded)` tag for skills already loaded into the
//     session.
//
// Returns "" when nothing to render so the runtime skips the empty
// section.
func (e *Extension) AdvertiseSystemPrompt(ctx context.Context, state extension.SessionState) string {
	h := FromState(state)
	if h == nil || h.manager == nil {
		return ""
	}
	var parts []string
	if meta := renderLoadedSkillsMeta(h); meta != "" {
		parts = append(parts, meta)
	}
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

// renderLoadedSkillsMeta produces the per-loaded-skill metadata
// block: directory path + bundled files listing (scripts /
// references / assets, only the categories that are non-empty).
// Phase 4.2 §3.4 — lets the model invoke `${SKILL_DIR}/scripts/foo.py`
// via existing bash:run / python:run_script providers.
//
// Inline skills (no on-disk Root) emit only the name + description
// header — there are no bundled files to list.
func renderLoadedSkillsMeta(h *SessionSkill) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if len(h.loaded) == 0 {
		return ""
	}
	names := make([]string, 0, len(h.loaded))
	for n := range h.loaded {
		names = append(names, n)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("## Loaded skill bundles\n\n")
	for _, n := range names {
		sk := h.loaded[n]
		b.WriteString("Loaded skill: `")
		b.WriteString(sk.Manifest.Name)
		b.WriteString("`\n")
		if sk.Root != "" {
			b.WriteString("  directory: ")
			b.WriteString(sk.Root)
			b.WriteString("\n")
		}
		if desc := strings.TrimSpace(sk.Manifest.Description); desc != "" {
			b.WriteString("  description: ")
			b.WriteString(desc)
			b.WriteString("\n")
		}
		if sk.FS != nil {
			writeBundleCategory(&b, sk.FS, "scripts")
			writeBundleCategory(&b, sk.FS, "references")
			writeBundleCategory(&b, sk.FS, "assets")
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// writeBundleCategory enumerates files under a single subdirectory
// of the skill bundle (scripts/, references/, assets/) and renders
// them as a sorted bullet list. Silent when the category dir is
// missing or empty — keeps the prompt block tight.
func writeBundleCategory(b *strings.Builder, sfs fs.FS, category string) {
	var paths []string
	_ = fs.WalkDir(sfs, category, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// Category root absent → fs.WalkDir returns the error
			// from the initial Stat. Swallow — empty category is
			// the common case.
			if p == category {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		paths = append(paths, p)
		return nil
	})
	if len(paths) == 0 {
		return
	}
	sort.Strings(paths)
	b.WriteString("  ")
	b.WriteString(category)
	b.WriteString(":\n")
	for _, p := range paths {
		b.WriteString("    - ")
		b.WriteString(p)
		b.WriteString("\n")
	}
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
//
// Phase-4.2 tri-state union: union resolution happens implicitly at
// the bindings layer (pkg/extension/skill/extension.go::collectBindings).
// A loaded skill with absent allowed-tools (Manifest.AllowedTools ==
// nil) contributes nothing to Bindings.AllowedTools; admission is
// "any loaded skill admits this tool", so absent skills don't reduce
// the catalogue but also don't extend it on their own — equivalent
// to "inheriting the union of other loaded skills' explicit grants".
// No special FilterTools logic needed for the union.
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
	skillFound, role, err := lookupSkillRole(ctx, state, skillName, roleName)
	if err != nil {
		return extension.SubagentUnknown, err
	}
	if !skillFound {
		return extension.SubagentUnknown, nil
	}
	if roleName == "" || role != nil {
		return extension.SubagentValid, nil
	}
	return extension.SubagentSkillFoundRoleMissing, nil
}

// SubagentSpawnHint implements [extension.SubagentSpawnHinter]. Returns
// the manifest-declared Intent for (skill, role) — empty string when
// not set, when the skill / role is unknown, or when called without a
// role (skill-level calls have no role to inspect). Spawn-time
// validation against the runtime's model router is the caller's job
// (tools_subagent_spawn): a typo here surfaces as a "intent unknown"
// warn at spawn, not as a model_unavailable error on the child's
// first turn.
func (e *Extension) SubagentSpawnHint(ctx context.Context, state extension.SessionState, skillName, roleName string) (extension.SubagentSpawnHint, error) {
	if roleName == "" {
		return extension.SubagentSpawnHint{}, nil
	}
	_, role, err := lookupSkillRole(ctx, state, skillName, roleName)
	if err != nil || role == nil {
		return extension.SubagentSpawnHint{}, err
	}
	return extension.SubagentSpawnHint{Intent: role.Intent}, nil
}

// lookupSkillRole walks the loaded skill catalog once to locate a
// (skill, role) pair. Returns (skillFound, role, err):
//   - skillFound is true iff a skill with skillName exists in the
//     catalog (regardless of role match).
//   - role is non-nil when both the skill and the named role exist.
//   - err propagates manager.List failures verbatim.
//
// Empty roleName short-circuits role lookup (skillFound only).
// The manager-less path (no SkillManager wired — fixture / no-skill
// tests) returns (false, nil, nil) so callers fall back to their
// no-validation default.
func lookupSkillRole(ctx context.Context, state extension.SessionState, skillName, roleName string) (bool, *skillpkg.SubAgentRole, error) {
	h := FromState(state)
	if h == nil || h.manager == nil {
		return false, nil, nil
	}
	all, err := h.manager.List(ctx)
	if err != nil {
		return false, nil, err
	}
	for _, s := range all {
		if s.Manifest.Name != skillName {
			continue
		}
		if roleName == "" {
			return true, nil, nil
		}
		for i := range s.Manifest.Hugen.SubAgents {
			r := &s.Manifest.Hugen.SubAgents[i]
			if r.Name == roleName {
				return true, r, nil
			}
		}
		return true, nil, nil
	}
	return false, nil, nil
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

