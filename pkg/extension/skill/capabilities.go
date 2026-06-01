package skill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

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
//
// Mission-PDCA (design 003): the MissionDispatcher / MissionStartLookup
// capabilities have moved to pkg/extension/mission. Skill ext no
// longer participates in mission dispatch; the manifest's
// `metadata.hugen.mission.{enabled,on_start,summary}` fields are
// inert under the new model — the PDCA shape lives in
// `metadata.hugen.mission.plan.*` and mission ext owns parsing it.
var (
	_ extension.Advertiser          = (*Extension)(nil)
	_ extension.ToolFilter          = (*Extension)(nil)
	_ extension.GenerationProvider  = (*Extension)(nil)
	_ extension.ToolPolicyAdvisor   = (*Extension)(nil)
	_ extension.SubagentDescriber   = (*Extension)(nil)
	_ extension.SubagentSpawnHinter = (*Extension)(nil)
	_ extension.SubagentSpawnApplier = (*Extension)(nil)
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
	// Phase 5.2 γ — split advertise tokens into loaded vs
	// catalogue so the UI can show "this is the cost of skills
	// you actually loaded" separately from "this is what the
	// model sees as the discovery menu".
	loadedTokens, catalogTokens := h.AdvertiseSplit()
	if loadedTokens > 0 {
		body["loaded_skill_tokens"] = loadedTokens
	}
	if catalogTokens > 0 {
		body["available_skill_tokens"] = catalogTokens
	}
	if total := loadedTokens + catalogTokens; total > 0 {
		// β legacy field — kept for adapters that haven't
		// learned the split shape yet.
		body["advertise_tokens"] = total
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil
	}
	return data
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
		Skip:         n.Skip,
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
	// Phase 5.2 γ — score each render contribution separately so
	// the context-budget UI can split "skill bodies you loaded"
	// from "catalogue advertising more skills available".
	//
	// Phase 6.x — the catalogue half migrated to the
	// ModelInTurnAdvisor turn_preamble (rendered by [TurnPreamble]
	// and injected before the last user message for recency +
	// prompt-cache). This method now keeps only the rendered skill
	// bodies + recommended-tags advice. The loaded-skill bundle
	// listing (references / scripts / assets) ALSO moved to
	// turn_preamble (dogfood: roles never loaded refs when the
	// listing sat atop a long system prompt) so it rides next to the
	// catalogue with the "load the references you need" directive,
	// near the model's decision point.
	var (
		parts        []string
		loadedTokens int
	)
	// Available missions — moved to pkg/extension/mission's
	// Advertiser. Mission-PDCA (design 003).
	if b, err := h.Bindings(ctx); err == nil && b.Instructions != "" {
		parts = append(parts, b.Instructions)
		loadedTokens += extension.EstimateTokens(b.Instructions)
	}
	// Phase 4.2.3 Block A — recommended notepad tags advertised
	// by the loaded mission dispatcher(s). Empty when no loaded
	// skill is mission-enabled or carries a tag list.
	if tags := renderNotepadTagAdvice(renderer, h); tags != "" {
		parts = append(parts, tags)
		loadedTokens += extension.EstimateTokens(tags)
	}
	h.SetLoadedTokens(loadedTokens)
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n")
}

// TurnPreamble implements [extension.ModelInTurnAdvisor]: it renders
// the AVAILABLE-skills catalogue (the dynamic advertise) for injection
// just before the last user message each turn, instead of baking it
// into the system prompt. Two wins (Phase 6.x): recency — weak models
// attend to content near the ask, not the top of a long system prompt
// (the dogfood failure where the catalogue was ignored) — and prompt
// cache, since the system prompt becomes stable while this volatile
// block rides after the cache boundary.
//
// The scoped/full split mirrors the pre-6.x AdvertiseSystemPrompt
// logic: a session opened with a recipe allow-list (Phase 6.1d) sees
// the catalogue narrowed to `loaded ∪ allowed_skills`; others see the
// full catalogue. Records the catalogue token estimate via
// [SessionSkill.SetCatalogTokens] so the context-budget UI still
// splits loaded vs catalogue. Returns "" when nothing to advertise.
func (e *Extension) TurnPreamble(ctx context.Context, state extension.SessionState) string {
	h := FromState(state)
	if h == nil || h.manager == nil {
		return ""
	}
	renderer := state.Prompts()
	var cat string
	if allowedList, scoped := allowedSkillsFromState(state); scoped {
		cat = renderCatalogueFiltered(ctx, renderer, h, allowedList)
	} else {
		cat = renderCatalogue(ctx, renderer, h)
	}
	// Phase 6.x dogfood follow-up — the loaded-skill bundle listing
	// (references + scripts + assets) rides here, right after the
	// catalogue, carrying the directive to `skill:ref` the references
	// the model needs. Co-locating it with the catalogue near the
	// user ask (vs buried atop the system prompt) is what gets weak
	// models to actually open the docs instead of guessing query /
	// filter syntax.
	parts := make([]string, 0, 2)
	if cat != "" {
		parts = append(parts, cat)
	}
	if meta := renderLoadedSkillsMeta(h); meta != "" {
		parts = append(parts, meta)
	}
	out := strings.Join(parts, "\n\n")
	h.SetCatalogTokens(extension.EstimateTokens(out))
	return out
}

// OnToolResult implements [extension.ModelInTurnAdvisor]: it walks the
// session's LOADED skills, gathers their `metadata.hugen.hints` of
// type on_tool_result, matches each against the tool result (name glob
// + optional structured Code + optional result-text regex), and
// returns the matching hint messages joined — the session folds them
// inline into the result the model reads next, with no separate emitted
// frame. Fed EVERY tool result (runtime error and successful dispatch
// alike); the hint's Code / regex do the discriminating, so the runtime
// never pre-classifies error-vs-success from the body.
//
// Only LOADED skills contribute (a hint is guidance for a skill the
// model is actively using); merely-installed skills stay silent.
// Multiple matches across loaded skills compose, de-duped and capped.
func (e *Extension) OnToolResult(ctx context.Context, state extension.SessionState, ev extension.ToolResultEvent) string {
	h := FromState(state)
	if h == nil {
		return ""
	}
	h.mu.RLock()
	names := make([]string, 0, len(h.loaded))
	for n := range h.loaded {
		names = append(names, n)
	}
	skills := make(map[string]skillpkg.Skill, len(h.loaded))
	for n, sk := range h.loaded {
		skills[n] = sk
	}
	h.mu.RUnlock()
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names) // stable contribution order
	var (
		out  []string
		seen = map[string]struct{}{}
	)
	for _, n := range names {
		for _, hint := range skills[n].Manifest.Hugen.Hints {
			msg := hint.MatchToolResult(ev.Tool, ev.Code, ev.ResultText)
			if msg == "" {
				continue
			}
			if _, dup := seen[msg]; dup {
				continue
			}
			seen[msg] = struct{}{}
			out = append(out, msg)
			if len(out) >= maxToolResultHints {
				return strings.Join(out, "\n\n")
			}
		}
	}
	return strings.Join(out, "\n\n")
}

// maxToolResultHints caps how many distinct hint messages a single
// tool result can carry, so a session with many loaded skills can't
// balloon one result into a wall of steer.
const maxToolResultHints = 3

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
// any loaded skill advertises via metadata.hugen.notepad.tags.
//
// Walks every loaded skill — categories vary with which skills
// are currently loaded; each skill teaches the model what shape
// of fact belongs in the notepad for that domain. Universal
// chat / mission tags come from autoloaded system skills
// (`_root`, `_mission`); domain tags come from extensions (e.g.
// a data skill declaring `schema-finding`, `query-pattern`).
// De-dupes tag names — first hint wins, sort order is name-
// stable so the "first" is the alphabetically-first skill
// defining it; declaration order within each contributing skill
// is preserved. Empty when no loaded skill carries tag
// declarations.
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
		for _, t := range sk.Manifest.Hugen.Notepad.Tags {
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
	b.WriteString("These skills are loaded. Any reference docs and files they " +
		"bundle are listed below. When you need a loaded skill's detail, LOAD " +
		"the relevant reference with `skill:ref` (args: `name`=<the skill>, " +
		"`ref`=<a path shown under references/ below, without the `.md`>) and " +
		"read it before relying on assumptions. Files under `scripts/` run via " +
		"the bundled execution tools at the `directory` path shown.\n\n")
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
	return renderCatalogueScoped(ctx, renderer, h, nil)
}

// renderCatalogueFiltered narrows [renderCatalogue] to entries
// that are EITHER in the `allowed` whitelist OR already loaded in
// the session. Used by sessions that were opened with a spawner-
// scoped allow-list — the recipe child sees its loaded baseline
// (system + worker + recipe + pre-loaded deps) tagged `(loaded)`
// alongside the whitelist entries reachable via `skill:load`.
// Phase 6.1d (additive interpretation — allow-list adds to the
// autoloaded baseline, never replaces it).
func renderCatalogueFiltered(ctx context.Context, renderer *prompts.Renderer, h *SessionSkill, allowed []string) string {
	allow := make(map[string]struct{}, len(allowed))
	for _, n := range allowed {
		allow[n] = struct{}{}
	}
	return renderCatalogueScoped(ctx, renderer, h, allow)
}

func renderCatalogueScoped(ctx context.Context, renderer *prompts.Renderer, h *SessionSkill, allow map[string]struct{}) string {
	all, err := h.manager.List(ctx)
	if err != nil || len(all) == 0 {
		return ""
	}
	loadedSet := map[string]struct{}{}
	for _, n := range h.LoadedNames(ctx) {
		loadedSet[n] = struct{}{}
	}
	type skillItem struct {
		Name          string
		Description   string
		Loaded        bool
		RecipeCatalog bool
	}
	items := make([]skillItem, 0, len(all))
	for _, sk := range all {
		// Phase 6.1d — task-eligible recipe skills are not listed
		// here. They surface to the model as synthetic
		// `task:<recipe-name>` tools (scheduler ext provider), and
		// loading them as regular skills is not part of the
		// task-runner flow — the category skill admits the synthetic
		// tool via its allowed-tools instead.
		if sk.Manifest.Hugen.Task.Eligible {
			continue
		}
		_, on := loadedSet[sk.Manifest.Name]
		if allow != nil {
			// Scoped session — show loaded skills (the baseline +
			// pre-loaded layer the LLM should know it already has)
			// PLUS whitelist entries it can still reach via
			// skill:load. Anything outside this union is hidden so
			// the LLM doesn't try to load it.
			_, inAllow := allow[sk.Manifest.Name]
			if !inAllow && !on {
				continue
			}
		}
		items = append(items, skillItem{
			Name:          sk.Manifest.Name,
			Description:   strings.TrimSpace(sk.Manifest.Description),
			Loaded:        on,
			RecipeCatalog: sk.Manifest.Hugen.RecipeCatalog,
		})
	}
	if len(items) == 0 {
		return ""
	}
	// Phase 6.1d — recipe catalogs (skills bundling tested `task:*`
	// recipes) sort to the top so the model spots them before the
	// long regular-skill tail; the constitution tells it to prefer
	// a matching catalog's recipe over hand-rolling the job. Stable
	// within each group, name-sorted, so the order is deterministic
	// across renders.
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].RecipeCatalog != items[j].RecipeCatalog {
			return items[i].RecipeCatalog
		}
		return items[i].Name < items[j].Name
	})
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
	allowed := allowedFromHandle(ctx, h, state)
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

// SubagentSpawnHint implements [extension.SubagentSpawnHinter].
// Returns the manifest-declared Intent for the spawn:
//
//   - When `role` is set, looks up `sub_agents[name=role].intent`
//     (the mission-style per-role override).
//   - When `role` is empty AND the dispatching skill declares
//     `metadata.hugen.task.eligible: true`, returns
//     `metadata.hugen.task.intent` as the hint (Phase 6.1d — recipe
//     children spawn through the task ext without a role; the
//     recipe's manifest decides the model intent).
//   - Otherwise returns empty hint.
//
// Spawn-time validation against the runtime's model router is the
// caller's job (applyChildIntent): a typo here surfaces as a
// "intent unknown" warn at spawn, not as a model_unavailable error
// on the child's first turn.
func (e *Extension) SubagentSpawnHint(ctx context.Context, state extension.SessionState, skillName, roleName string) (extension.SubagentSpawnHint, error) {
	if e.manager == nil || skillName == "" {
		return extension.SubagentSpawnHint{}, nil
	}
	if roleName == "" {
		// Recipe path — dispatching skill IS the recipe; intent
		// comes from its `task.intent` field.
		sk, err := e.manager.Get(ctx, skillName)
		if err != nil {
			return extension.SubagentSpawnHint{}, nil
		}
		tb := sk.Manifest.Hugen.Task
		if tb.Eligible && tb.Intent != "" {
			return extension.SubagentSpawnHint{Intent: tb.Intent}, nil
		}
		return extension.SubagentSpawnHint{}, nil
	}
	_, role, err := lookupSkillRole(ctx, state, skillName, roleName)
	if err != nil || role == nil {
		return extension.SubagentSpawnHint{}, err
	}
	return extension.SubagentSpawnHint{Intent: role.Intent}, nil
}

// ApplyOnSubagentSpawn implements [extension.SubagentSpawnApplier].
// Reads `sub_agents[*].autoload_skills` from the dispatching
// skill's manifest (via the SkillManager — the skill does NOT need
// to be loaded on the child) and Load()s each on the child's
// per-session SessionSkill. Per-skill Load failures are joined into
// the return value but the loop continues so one bad autoload entry
// does not deny the worker the rest of its base surface; the
// runtime logs the joined error and proceeds with the spawn (the
// worker can still skill:load(...) at runtime for anything missing).
//
// Tier compatibility is enforced by SessionSkill.Load itself —
// each target's tier_compatibility must include the child's tier
// or Load returns ErrTierForbidden, which surfaces here joined into
// the return value.
func (e *Extension) ApplyOnSubagentSpawn(ctx context.Context, child extension.SessionState, skillName, roleName string) error {
	if skillName == "" || roleName == "" || e.manager == nil {
		return nil
	}
	sk, err := e.manager.Get(ctx, skillName)
	if err != nil {
		// Unknown dispatching skill — silently no-op. Spawn-time
		// validation against the catalogue runs elsewhere; an
		// unrecognised skill here is not this applier's failure
		// to surface.
		return nil
	}
	var role *skillpkg.SubAgentRole
	roles := sk.Manifest.Hugen.SubAgents
	for i := range roles {
		if roles[i].Name == roleName {
			role = &roles[i]
			break
		}
	}
	if role == nil || len(role.AutoloadSkills) == 0 {
		return nil
	}
	h := FromState(child)
	if h == nil {
		return nil
	}
	var loadErrs []error
	for _, name := range role.AutoloadSkills {
		if err := h.Load(ctx, name); err != nil {
			loadErrs = append(loadErrs, fmt.Errorf("autoload %q for role %s/%s: %w", name, skillName, roleName, err))
		}
	}
	if len(loadErrs) == 0 {
		return nil
	}
	return errors.Join(loadErrs...)
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
// an empty (non-nil) set when the session has no loaded skills
// and no role-scoped grants; populated otherwise.
//
// Role-scoped grants: when state carries a (Skill, Role) pair
// resolving to a SubAgentRole in the manager, that role's Tools
// field augments the allow-set on top of the loaded-skill union.
// This is the wiring under design 003 that lets per-role surfaces
// declared in the dispatching skill's manifest (e.g. analyst
// planner role granting `mission:validate_plan`) actually reach
// the worker's snapshot WITHOUT auto-loading the dispatching skill.
func allowedFromHandle(ctx context.Context, h *SessionSkill, state extension.SessionState) *allowedSet {
	if h == nil {
		return nil
	}
	out := &allowedSet{
		exact:             map[string]bool{},
		approvalExact:     map[string]struct{}{},
		approvalProviders: map[string]struct{}{},
	}
	b, err := h.Bindings(ctx)
	if err == nil {
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
	}
	if state != nil {
		mergeRoleTools(ctx, h, state, out)
	}
	return out
}

// mergeRoleTools looks up the SubAgentRole that matches the
// session's (Skill, Role) pair in the dispatching skill's manifest
// and adds its Tools entries to allow. Silent no-op when:
//
//   - state has no Role / no Skill (root sessions, externally-
//     spawned workers without skill metadata),
//   - the manager doesn't know the dispatching skill,
//   - the role name isn't in the manifest's sub_agents block.
//
// requires_approval flags on role.Tools entries are honoured the
// same as loaded-skill grants — keeps the approval surface
// consistent across both sources.
func mergeRoleTools(ctx context.Context, h *SessionSkill, state extension.SessionState, allow *allowedSet) {
	if h == nil || h.manager == nil || state == nil {
		return
	}
	skillName := state.Skill()
	roleName := state.Role()
	if skillName == "" || roleName == "" {
		return
	}
	sk, err := h.manager.Get(ctx, skillName)
	if err != nil {
		return
	}
	var role *skillpkg.SubAgentRole
	for i := range sk.Manifest.Hugen.SubAgents {
		if sk.Manifest.Hugen.SubAgents[i].Name == roleName {
			role = &sk.Manifest.Hugen.SubAgents[i]
			break
		}
	}
	if role == nil {
		return
	}
	for _, g := range role.Tools {
		for _, t := range g.Tools {
			full := g.Provider + ":" + t
			if strings.HasSuffix(t, "*") {
				allow.patterns = append(allow.patterns, strings.TrimSuffix(full, "*"))
				continue
			}
			allow.exact[full] = true
		}
		for _, name := range g.RequiresApproval {
			if name == "*" {
				allow.approvalProviders[g.Provider] = struct{}{}
				continue
			}
			allow.approvalExact[g.Provider+":"+name] = struct{}{}
		}
	}
}
