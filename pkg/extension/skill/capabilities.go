package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"text/template"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/prompts"
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
	_ extension.Advertiser          = (*Extension)(nil)
	_ extension.ToolFilter          = (*Extension)(nil)
	_ extension.GenerationProvider  = (*Extension)(nil)
	_ extension.ToolPolicyAdvisor   = (*Extension)(nil)
	_ extension.SubagentDescriber   = (*Extension)(nil)
	_ extension.SubagentSpawnHinter = (*Extension)(nil)
	_ extension.MissionDispatcher   = (*Extension)(nil)
	_ extension.MissionStartLookup  = (*Extension)(nil)
	_ extension.CloseTurnLookup     = (*Extension)(nil)
	_ extension.StatusReporter      = (*Extension)(nil)
)

// ReportStatus implements [extension.StatusReporter]. Returns the
// sorted list of skills currently loaded into the calling session
// plus the count of tools visible at the moment. Wire shape:
//
//	{"loaded": [names...], "tools": N}
//
// Phase 5.1b shape (`loaded` only) is preserved for older consumers;
// `tools` is additive and optional. The tool count is computed via
// ToolManager.Snapshot which honours per-skill allowed-tools
// narrowing — what the model would actually see this turn.
func (e *Extension) ReportStatus(ctx context.Context, state extension.SessionState) json.RawMessage {
	h := FromState(state)
	if h == nil {
		return nil
	}
	names := h.LoadedNames(ctx)
	body := map[string]any{"loaded": names}
	if tm := state.Tools(); tm != nil {
		if snap, err := tm.Snapshot(ctx, state.SessionID()); err == nil {
			body["tools"] = len(snap.Tools)
		}
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil
	}
	return data
}

// MissionSkillExists implements [extension.MissionDispatcher].
// Returns (true, nil) when the named skill is installed AND its
// manifest declares metadata.hugen.mission.enabled:true. Phase
// 4.2.2 §6.
func (e *Extension) MissionSkillExists(ctx context.Context, skill string) (bool, error) {
	if skill == "" || e.manager == nil {
		return false, nil
	}
	all, err := e.manager.List(ctx)
	if err != nil {
		return false, err
	}
	for _, sk := range all {
		if sk.Manifest.Name == skill && sk.Manifest.Hugen.Mission.Enabled {
			return true, nil
		}
	}
	return false, nil
}

// missionStartTemplateData is the fixed-vocabulary context the
// on_start templates render against. Limited vocabulary by design
// — complex logic lives in code, not in templates. Phase 4.2.2 §7.
type missionStartTemplateData struct {
	UserGoal    string
	ParentSkill string
	Inputs      any
}

// ResolveMissionStart implements [extension.MissionStartLookup].
// Looks up the named skill, returns nil if it is not mission-enabled
// or declares no on_start, otherwise renders the on_start templates
// against the supplied (goal, inputs) and returns the post-render
// MissionStartBlock the runtime applies. Phase 4.2.2 §7.
func (e *Extension) ResolveMissionStart(ctx context.Context, skill, goal string, inputs any) (*extension.MissionStartBlock, error) {
	if skill == "" || e.manager == nil {
		return nil, nil
	}
	all, err := e.manager.List(ctx)
	if err != nil {
		return nil, err
	}
	var found *skillpkg.Skill
	for i, sk := range all {
		if sk.Manifest.Name == skill {
			found = &all[i]
			break
		}
	}
	if found == nil || !found.Manifest.Hugen.Mission.Enabled {
		return nil, nil
	}
	on := found.Manifest.Hugen.Mission.OnStart
	if on.Plan.BodyTemplate == "" && !on.Whiteboard.Init && on.FirstMessage.Template == "" {
		return nil, nil
	}
	data := missionStartTemplateData{
		UserGoal:    goal,
		ParentSkill: skill,
		Inputs:      inputs,
	}
	out := &extension.MissionStartBlock{
		PlanCurrentStep: on.Plan.CurrentStep,
		WhiteboardInit:  on.Whiteboard.Init,
	}
	if on.Plan.BodyTemplate != "" {
		body, err := renderMissionTemplate("plan.body_template", on.Plan.BodyTemplate, data)
		if err != nil {
			return nil, err
		}
		out.PlanText = body
	}
	if on.FirstMessage.Template != "" {
		msg, err := renderMissionTemplate("first_message.template", on.FirstMessage.Template, data)
		if err != nil {
			return nil, err
		}
		out.FirstMessageOverride = msg
	}
	return out, nil
}

// renderMissionTemplate runs `body` through text/template with the
// fixed mission-start vocabulary (UserGoal / ParentSkill / Inputs).
// Errors propagate with the field name so a malformed template
// surfaces a precise diagnostic at spawn time. Phase 4.2.2 §7.
func renderMissionTemplate(field, body string, data missionStartTemplateData) (string, error) {
	tpl, err := template.New(field).Parse(body)
	if err != nil {
		return "", fmt.Errorf("mission.on_start.%s: parse: %w", field, err)
	}
	var buf strings.Builder
	if err := tpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("mission.on_start.%s: execute: %w", field, err)
	}
	return buf.String(), nil
}

// ResolveCloseTurn implements [extension.CloseTurnLookup]. Walks
// the calling session's loaded skills and returns the most-
// specific on_close configuration. Phase 4.2.3 ε.
//
// Precedence (first non-zero block wins):
//
//  1. Sub-agent role override — a loaded skill's
//     metadata.hugen.sub_agents[i].on_close where i.Name matches
//     spawnRole.
//  2. Mission-level config — metadata.hugen.mission.on_close
//     on the dispatching skill (matched by spawnSkill).
//  3. Generic fallback — metadata.hugen.mission.on_close on any
//     other loaded skill that's not the dispatcher (typically
//     the autoloaded `_mission` or `_worker` base skill).
//
// Returns ({}, nil) when no loaded skill opts in. Caller gates
// via CloseTurnBlock.IsEmpty().
func (e *Extension) ResolveCloseTurn(_ context.Context, state extension.SessionState, spawnSkill, spawnRole string) (extension.CloseTurnBlock, error) {
	h := FromState(state)
	if h == nil {
		return extension.CloseTurnBlock{}, nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if len(h.loaded) == 0 {
		return extension.CloseTurnBlock{}, nil
	}

	// Stable iteration so the precedence-2 / precedence-3
	// fallback chain is deterministic across runs.
	names := make([]string, 0, len(h.loaded))
	for n := range h.loaded {
		names = append(names, n)
	}
	sort.Strings(names)

	// (1) role override — any loaded skill whose sub_agents
	// declare an entry matching spawnRole with a non-zero
	// on_close.
	if spawnRole != "" {
		for _, n := range names {
			sk := h.loaded[n]
			for _, role := range sk.Manifest.Hugen.SubAgents {
				if role.Name != spawnRole {
					continue
				}
				if role.OnClose.IsZero() {
					continue
				}
				return closeBlockFromManifest(role.OnClose), nil
			}
		}
	}

	// (2) dispatcher mission-level override.
	if spawnSkill != "" {
		if sk, ok := h.loaded[spawnSkill]; ok && !sk.Manifest.Hugen.Mission.OnClose.IsZero() {
			return closeBlockFromManifest(sk.Manifest.Hugen.Mission.OnClose), nil
		}
	}

	// (3) first generic fallback — any other loaded skill with
	// a non-zero mission.on_close. Stable order (name-sorted)
	// means `_mission` / `_worker` consistently win on tie when
	// no domain override is present.
	for _, n := range names {
		if n == spawnSkill {
			continue
		}
		sk := h.loaded[n]
		if sk.Manifest.Hugen.Mission.OnClose.IsZero() {
			continue
		}
		return closeBlockFromManifest(sk.Manifest.Hugen.Mission.OnClose), nil
	}

	return extension.CloseTurnBlock{}, nil
}

// closeBlockFromManifest projects the manifest's
// MissionOnClose into the runtime-facing CloseTurnBlock. Only
// the notepad sub-block is wired today; other sub-blocks land
// alongside without changing this shape.
func closeBlockFromManifest(c skillpkg.MissionOnClose) extension.CloseTurnBlock {
	n := c.Notepad
	return extension.CloseTurnBlock{
		SystemPrompt: n.Prompt,
		AllowedTools: append([]string(nil), n.AllowedTools...),
		MaxTurns:     n.MaxTurns,
		SkipIfIdle:   n.SkipIfIdle,
	}
}

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
	renderer := state.Prompts()
	var parts []string
	// Available missions — root-only block listing every
	// dispatch-eligible skill (mission.enabled:true) with its
	// Summary so the root model can pick a skill argument for
	// session:spawn_mission. Phase 4.2.2 §6.
	if h.tier == skillpkg.TierRoot {
		if missions := renderAvailableMissions(ctx, renderer, h); missions != "" {
			parts = append(parts, missions)
		}
	}
	if meta := renderLoadedSkillsMeta(h); meta != "" {
		parts = append(parts, meta)
	}
	if b, err := h.Bindings(ctx); err == nil && b.Instructions != "" {
		parts = append(parts, b.Instructions)
	}
	if cat := renderCatalogue(ctx, renderer, h); cat != "" {
		parts = append(parts, cat)
	}
	// Phase 4.2.3 Block A — recommended notepad tags advertised
	// by the loaded mission dispatcher(s). Empty when no loaded
	// skill is mission-enabled or carries a tag list.
	if tags := renderNotepadTagAdvice(renderer, h); tags != "" {
		parts = append(parts, tags)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n")
}

// renderAvailableMissions enumerates every skill in the manager's
// store with metadata.hugen.mission.enabled:true and renders a
// "## Available missions" prompt section listing each by name +
// Summary. Returns "" when no mission-enabled skills are installed
// — root then has no dispatch options and spawn_mission surfaces
// `no_mission_skill`. Phase 4.2.2 §6.
func renderAvailableMissions(ctx context.Context, renderer *prompts.Renderer, h *SessionSkill) string {
	all, err := h.manager.List(ctx)
	if err != nil {
		return ""
	}
	type missionItem struct {
		Name    string
		Summary string
	}
	var picked []missionItem
	for _, sk := range all {
		if !sk.Manifest.Hugen.Mission.Enabled {
			continue
		}
		summary := strings.TrimSpace(sk.Manifest.Hugen.Mission.Summary)
		if summary == "" {
			summary = strings.TrimSpace(sk.Manifest.Description)
		}
		picked = append(picked, missionItem{
			Name:    sk.Manifest.Name,
			Summary: summary,
		})
	}
	if len(picked) == 0 {
		return ""
	}
	sort.Slice(picked, func(i, j int) bool {
		return picked[i].Name < picked[j].Name
	})
	return strings.TrimRight(renderer.MustRender(
		"skill/available_missions",
		map[string]any{"Missions": picked},
	), "\n")
}

// renderLoadedSkillsMeta produces the per-loaded-skill metadata
// block: directory path + bundled files listing (scripts /
// references / assets, only the categories that are non-empty).
// Phase 4.2 §3.4 — lets the model invoke `${SKILL_DIR}/scripts/foo.py`
// via existing bash:run / python:run_script providers.
//
// Inline skills (no on-disk Root) emit only the name + description
// header — there are no bundled files to list.
// renderNotepadTagAdvice produces Block A — a "## Notepad —
// recommended tags" prompt section listing the notepad categories
// that any loaded skill advertises via
// metadata.hugen.mission.on_start.notepad.tags. Phase 4.2.3 §5.
//
// Walks every loaded skill (no mission.enabled filter — universal
// tags live on the autoloaded tier skill `_mission`, domain tags
// live on the dispatcher like `analyst` / `_general`). De-dupes
// tag names (first hint wins, sort order is name-stable so the
// "first" is the alphabetically-first skill defining it), and
// preserves declaration order within each contributing skill.
// Empty when no loaded skill carries tag declarations.
func renderNotepadTagAdvice(renderer *prompts.Renderer, h *SessionSkill) string {
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
	type tagItem struct {
		Name string
		Hint string
	}
	seen := map[string]struct{}{}
	var order []tagItem
	for _, n := range names {
		sk := h.loaded[n]
		for _, t := range sk.Manifest.Hugen.Mission.OnStart.Notepad.Tags {
			name := strings.TrimSpace(t.Name)
			if name == "" {
				continue
			}
			if _, present := seen[name]; present {
				continue
			}
			seen[name] = struct{}{}
			order = append(order, tagItem{
				Name: name,
				Hint: strings.TrimSpace(t.Hint),
			})
		}
	}
	if len(order) == 0 {
		return ""
	}
	return renderer.MustRender(
		"notepad/recommended_tags",
		map[string]any{"Tags": order},
	)
}

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
func renderCatalogue(ctx context.Context, renderer *prompts.Renderer, h *SessionSkill) string {
	all, err := h.manager.List(ctx)
	if err != nil || len(all) == 0 {
		return ""
	}
	loadedSet := map[string]struct{}{}
	for _, n := range h.LoadedNames(ctx) {
		loadedSet[n] = struct{}{}
	}
	type skillItem struct {
		Name        string
		Description string
		Loaded      bool
	}
	items := make([]skillItem, 0, len(all))
	for _, sk := range all {
		_, on := loadedSet[sk.Manifest.Name]
		items = append(items, skillItem{
			Name:        sk.Manifest.Name,
			Description: strings.TrimSpace(sk.Manifest.Description),
			Loaded:      on,
		})
	}
	return renderer.MustRender(
		"skill/catalogue",
		map[string]any{"Skills": items},
	)
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
		if !allowed.match(t.Name) {
			continue
		}
		// Phase 5.1 § η: tag the tool with the approval-gate
		// flag the loaded skills declared. The dispatcher reads
		// this per-session snapshot to decide whether to invoke
		// session:inquire(type=approval) before forwarding.
		if allowed.requiresApproval(t.Name) {
			t.RequiresApproval = true
		}
		out = append(out, t)
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
	// approvalExact + approvalProviders together model the
	// phase-5.1 § 2.6 "exact tool name OR '*' wildcard" matching
	// rule for requires_approval. approvalExact carries fully-
	// qualified names (provider:tool); approvalProviders names the
	// providers whose grant carried a `requires_approval: ['*']`
	// entry so every tool from that provider gates.
	approvalExact     map[string]struct{}
	approvalProviders map[string]struct{}
}

// requiresApproval reports whether the tool's fully-qualified
// name should trigger an approval gate. Exact match wins;
// otherwise a `*` wildcard at any of the loaded grants for the
// tool's provider matches.
func (a *allowedSet) requiresApproval(name string) bool {
	if a == nil {
		return false
	}
	if _, ok := a.approvalExact[name]; ok {
		return true
	}
	provider := name
	if i := strings.Index(name, ":"); i > 0 {
		provider = name[:i]
	}
	if _, ok := a.approvalProviders[provider]; ok {
		return true
	}
	return false
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
	out := &allowedSet{
		exact:             map[string]bool{},
		approvalExact:     map[string]struct{}{},
		approvalProviders: map[string]struct{}{},
	}
	for _, g := range b.AllowedTools {
		for _, t := range g.Tools {
			full := g.Provider + ":" + t
			if strings.HasSuffix(t, "*") {
				out.patterns = append(out.patterns, strings.TrimSuffix(full, "*"))
				continue
			}
			out.exact[full] = true
		}
		for _, name := range g.RequiresApproval {
			if name == "*" {
				out.approvalProviders[g.Provider] = struct{}{}
				continue
			}
			out.approvalExact[g.Provider+":"+name] = struct{}{}
		}
	}
	return out
}
