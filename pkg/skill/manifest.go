package skill

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"
	"text/template"
	"time"

	"github.com/oasdiff/yaml"
)

// Manifest is the parsed agentskills.io frontmatter plus hugen
// extensions extracted from metadata.hugen.*. Unknown top-level
// keys are preserved in Metadata so skills authored against a
// future spec revision still parse.
type Manifest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	License     string `json:"license"`

	// AllowedTools is tri-state per agentskills.io semantics. Slice
	// nil-vs-empty is load-bearing — UnmarshalJSON preserves the
	// distinction explicitly:
	//
	//   - nil                          — manifest has no
	//                                    `allowed-tools` key. Skill
	//                                    claims no explicit grants
	//                                    of its own; under union
	//                                    resolution it inherits the
	//                                    catalogue every other
	//                                    loaded skill admits. Used
	//                                    by community skills
	//                                    authored against the
	//                                    agentskills.io standard
	//                                    (absent = "do not
	//                                    restrict").
	//   - non-nil, len(...) == 0       — manifest has explicit
	//                                    `allowed-tools: []`.
	//                                    Reference-only skill:
	//                                    contributes nothing to
	//                                    the model-facing tool
	//                                    catalogue.
	//   - non-nil, len(...) > 0        — explicit grants; admits
	//                                    exactly those tools.
	//
	// Probe with `m.AllowedTools == nil` for absent;
	// `len(m.AllowedTools) == 0 && m.AllowedTools != nil` for
	// explicit empty; `len(m.AllowedTools) > 0` for populated.
	//
	// See design/002-runtime-canonical/phase-4.2-spec.md §3.1.
	AllowedTools AllowedTools   `json:"allowed-tools,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`

	// Hugen is the typed projection of metadata.hugen.* — populated
	// after Parse runs the raw YAML through extractHugen. Authors
	// don't write Hugen directly; they put the data under
	// metadata.hugen in the manifest.
	Hugen HugenMetadata `json:"-"`

	// Body is the SKILL.md content after the closing `---`. Empty
	// when the file is frontmatter-only.
	Body []byte `json:"-"`

	// Raw is the original frontmatter bytes (between the two `---`
	// lines, exclusive). Useful for re-emitting / round-trip tests.
	Raw []byte `json:"-"`
}

// ToolGrant declares which tools of a given provider the skill
// makes available to the LLM when loaded.
type ToolGrant struct {
	Provider string   `json:"provider"`
	Tools    []string `json:"tools"`
	// RequiresApproval narrows the per-grant tools that the
	// runtime should intercept with a session:inquire approval
	// flow before forwarding the call to the provider. Phase 5.1
	// § 2.6 ships exact-name + '*'-wildcard matching only; no
	// glob support, no content-based gating.
	//
	// Names must either appear verbatim in the entry's Tools
	// list or be the literal '*' (which expands to every tool in
	// the same grant). Mixed lists are allowed. hugen-skill-
	// validate enforces this at manifest authoring time;
	// runtime ignores entries with unknown names (defence in
	// depth).
	RequiresApproval []string `json:"requires_approval,omitempty" yaml:"requires_approval,omitempty"`
}

// AllowedTools is the parsed allowed-tools list. Two manifest
// notations decode into the same []ToolGrant:
//
//	# grouped — agentskills.io spec form
//	allowed-tools:
//	  - provider: bash-mcp
//	    tools: [bash.run, bash.read_file]
//
//	# flat — hugen shorthand, "provider:tool" entries
//	allowed-tools:
//	  - bash-mcp:bash.run
//	  - bash-mcp:bash.read_file
//	  - skill:load
//
// Mixed lists are supported. Flat entries that share a provider
// merge into a single ToolGrant for that provider.
type AllowedTools []ToolGrant

// UnmarshalJSON decodes either notation into the same []ToolGrant
// shape. agentskills.io strict mode still gets a 1:1 round-trip
// because grouped entries are decoded directly into ToolGrant.
func (a *AllowedTools) UnmarshalJSON(data []byte) error {
	if len(bytes.TrimSpace(data)) == 0 || string(bytes.TrimSpace(data)) == "null" {
		*a = nil
		return nil
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("allowed-tools: must be a list: %w", err)
	}
	out := make([]ToolGrant, 0, len(raw))
	byProvider := map[string]int{} // provider → index in out
	for i, item := range raw {
		trimmed := bytes.TrimSpace(item)
		if len(trimmed) == 0 {
			return fmt.Errorf("allowed-tools[%d]: empty entry", i)
		}
		switch trimmed[0] {
		case '"':
			var entry string
			if err := json.Unmarshal(item, &entry); err != nil {
				return fmt.Errorf("allowed-tools[%d]: %w", i, err)
			}
			j := strings.Index(entry, ":")
			if j <= 0 || j == len(entry)-1 {
				return fmt.Errorf(`allowed-tools[%d]: %q must be "provider:tool"`, i, entry)
			}
			provider, name := entry[:j], entry[j+1:]
			if idx, ok := byProvider[provider]; ok {
				out[idx].Tools = append(out[idx].Tools, name)
			} else {
				byProvider[provider] = len(out)
				out = append(out, ToolGrant{Provider: provider, Tools: []string{name}})
			}
		case '{':
			var grant ToolGrant
			if err := json.Unmarshal(item, &grant); err != nil {
				return fmt.Errorf("allowed-tools[%d]: %w", i, err)
			}
			out = append(out, grant)
			// Don't merge object-form entries into byProvider — the
			// author may intentionally split a provider across two
			// blocks (e.g. group commentary), and we want the
			// round-trip to preserve that shape.
		default:
			return fmt.Errorf("allowed-tools[%d]: expected string or object, got %s", i, string(trimmed))
		}
	}
	*a = out
	return nil
}

// HugenMetadata is the typed view of metadata.hugen.* — the
// hugen-specific extensions inside the manifest.
type HugenMetadata struct {
	// Requires is the legacy phase-3 spelling of the transitive
	// skill-dependency declaration. RequiresSkills (phase-4) is the
	// canonical name; both fields merge in AllRequires for the
	// closure resolver.
	Requires []string `json:"requires,omitempty"`

	// RequiresSkills is the phase-4 canonical name for transitive
	// skill dependencies (phase-4-spec §3 step 8 / §4.4 / Q19). The
	// resolver walks the closure with DFS + cycle detection
	// (ErrSkillCycle) at SkillManager.Load. Dependencies stay
	// loaded for the session's lifetime — there's no refcount on
	// unload.
	RequiresSkills []string `json:"requires_skills,omitempty" yaml:"requires_skills,omitempty"`

	// AllowedSkills is a runtime-load whitelist scoped to children
	// spawned with this skill as their dispatching manifest. When
	// the task extension's dispatch path stamps a recipe child with
	// this list (via SessionAllowedSkillsKey), the skill extension
	// restricts `skill:load` + the Available-skills catalogue to
	// entries in the list (plus the universal `_system` / `_worker`
	// baseline). Empty / absent on a recipe = "fixed surface: only
	// pre-loaded RequiresSkills are available". RequiresSkills
	// entries get loaded eagerly at spawn; AllowedSkills entries are
	// reachable via `skill:load` lazily — useful for skills with
	// heavy boot cost that the recipe needs only on certain
	// branches. Phase 6.1d.
	AllowedSkills []string `json:"allowed_skills,omitempty" yaml:"allowed_skills,omitempty"`

	Intents   []string                  `json:"intents,omitempty"`
	SubAgents []SubAgentRole            `json:"sub_agents,omitempty"`
	Memory    map[string]MemoryCategory `json:"memory,omitempty"`

	// Notepad declares which categories the model loading this
	// skill is encouraged to use when calling notepad:append. The
	// skill extension walks every loaded skill's Notepad.Tags and
	// renders them into the session's system prompt (Block A) with
	// a call-shape header — universal across tiers. Categories
	// vary with which skills are currently loaded: each skill
	// teaches the model what shape of fact belongs in the
	// notepad for that domain.
	Notepad NotepadBlock `json:"notepad,omitempty" yaml:"notepad,omitempty"`

	// Mission, when Enabled, declares the skill as a mission
	// dispatcher: root sees its Summary in the "Available
	// missions" prompt block and may pass it to session:spawn_mission.
	// Phase 4.2.2 §6. The block is enforceable only on extensions
	// (non-`_` names) — system skills are runtime primitives, not
	// dispatch targets.
	Mission MissionBlock `json:"mission,omitempty" yaml:"mission,omitempty"`

	// Task, when Eligible, declares the skill as task-eligible:
	// `schedule:create` may bind a recurring schedule to it (Phase 6
	// §0.5.4). Surface is purely declarative — the TaskManager
	// extension reads InputsSchema for JSON-Schema validation at
	// create time and AllowedToolsDefault as the default tool
	// allow-list operator can override in the approval modal
	// (6.1c). Empty / Eligible=false → skill is invisible to
	// `schedule:create`.
	Task TaskBlock `json:"task,omitempty" yaml:"task,omitempty"`

	// MaxTurns / MaxTurnsHard / StuckDetection are conceptually
	// per-session-tier, not per-skill — they tune the turn-loop and
	// stuck-detect heuristics, which depend on the session's role
	// (root=routing, mission=coordination, worker=execution), not on
	// which skill is loaded. They live on the manifest today as a
	// phase-4 artefact, when sessions had no tier.
	//
	// DEFERRED to phase 5: migrate to per-tier defaults in
	// config.yaml.session.tier_defaults + per-role overrides on
	// SubAgentRole. Until then the runtime keeps the current
	// "max across loaded skills" composition with bumped defaults
	// (defaultMaxToolIterations = 40, hard = 80) sized for the
	// 3-tier topology landed in phase 4.2.2.

	// MaxTurns is the per-skill cap on the model→tool→model loop
	// inside a single user turn. 0 (absent) defers to the runtime
	// default (defaultMaxToolIterations).
	MaxTurns int `json:"max_turns,omitempty" yaml:"max_turns,omitempty"`

	// MaxTurnsHard is the per-skill hard ceiling on the model→tool
	// →model loop, after which the runtime calls
	// Manager.Terminate(self, "hard_ceiling") rather than soft-
	// nudge the model. 0 (absent) defers to defaultMaxToolIterations
	// * 2. See phase-4-spec §8.2.
	MaxTurnsHard int `json:"max_turns_hard,omitempty" yaml:"max_turns_hard,omitempty"`

	// StuckDetection tunes the per-pattern detectors operating
	// independently of the soft/hard caps (repeated_hash,
	// tight_density, no_progress). All detectors default to
	// conservative values when the field is absent. See phase-4-spec
	// §8.3.
	StuckDetection StuckDetectionPolicy `json:"stuck_detection,omitempty" yaml:"stuck_detection,omitempty"`

	// Autoload, when true, tells the SessionManager to load this
	// skill into every newly opened session whose tier appears in
	// AutoloadFor. Loading is idempotent — manual /skill load of
	// the same name is a no-op.
	//
	// Reserved for system skills: a manifest with autoload:true
	// whose Name does not begin with "_" is rejected at parse time
	// (phase 4.2.2 §1). The "_" prefix is the structural marker
	// that distinguishes core/runtime skills from extensions.
	Autoload bool `json:"autoload,omitempty" yaml:"autoload,omitempty"`

	// AutoloadFor is the list of tiers in which Autoload fires.
	// Recognised values: "root", "mission", "worker" (phase 4.2.2
	// §2). An entry outside that set is rejected at parse time.
	// Required when Autoload is true; the runtime never infers a
	// default tier — authors declare placement deliberately.
	AutoloadFor []string `json:"autoload_for,omitempty" yaml:"autoload_for,omitempty"`

	// Compactor is the optional per-mission override for the
	// phase-5.2 history compactor. Applied when this skill is the
	// dispatching mission for the active session (mission tier).
	// Pointer so an absent block is distinct from
	// "block present with all defaults". Fields inside are also
	// pointers — the three-layer resolver
	// (top-level → tier → mission → role) only overwrites
	// explicitly-set values. See
	// design/004-runtime-post-phase-i/phase-5.2-compactor-spec.md §4.4.
	Compactor *CompactorOverride `json:"compactor,omitempty" yaml:"compactor,omitempty"`

	// TierCompatibility lists the tiers where the skill may be
	// loaded at all, whether via autoload or via explicit
	// skill:load. Outside this set the skill is invisible: it does
	// not surface in the per-turn skill advertise and a direct
	// skill:load surfaces tool_error{code:"tier_forbidden"}.
	// Empty defaults to ["worker"] — the safest tier where domain
	// tools are useful. Invariant: AutoloadFor ⊆ TierCompatibility
	// (a skill cannot auto-load where it would be forbidden
	// manually). Phase 4.2.2 §3.
	TierCompatibility []string `json:"tier_compatibility,omitempty" yaml:"tier_compatibility,omitempty"`

	// Hints is an extensible, typed list of in-turn advisories the
	// skill contributes while loaded. Each entry's Type selects a
	// [extension.ModelInTurnAdvisor] variation; the active one is
	// `on_tool_result`, which appends Message inline to a matching tool
	// result (error or success alike — see [HintTypeOnToolResult]). The
	// legacy `on_tool_error` parses as a deprecated alias of it. One
	// umbrella key — future variations (e.g. pre_tool_call) add a new
	// Type to the SAME list rather than a new top-level key. Unknown
	// Type → validate warns + the runtime ignores it (forward-compat).
	Hints []Hint `json:"hints,omitempty" yaml:"hints,omitempty"`
}

// HintType discriminates a [Hint] across the
// [extension.ModelInTurnAdvisor] variations.
type HintType = string

const (
	// HintTypeOnToolResult appends Message inline (same turn) to a
	// tool result whose content matches the hint, matched by tool-name
	// glob + optional structured Code + optional regex over the result
	// body. It is the single in-turn corrective-hint type: it fires on
	// EVERY tool result — runtime error and successful dispatch alike —
	// because the runtime no longer guesses error-vs-success from the
	// body (a clean result can carry "is_error":true / "ok":false in
	// its data; "errors":null is a success). The hint's regex / Code do
	// the discriminating. Covers both a runtime error a skill wants to
	// steer (match by Code) and a success-envelope failure or nudge
	// (match by regex: a GraphQL `Cannot query field` / a Hugr
	// `{"ok":false}` rejection / an `is_truncated:true` → file output).
	HintTypeOnToolResult HintType = "on_tool_result"

	// HintTypeOnToolError is the deprecated former split: a hint that
	// fired only on results the runtime body-classified as failures.
	// Kept as a parse-time alias (Parse normalises it to
	// HintTypeOnToolResult) so existing manifests keep working; new
	// manifests should use on_tool_result.
	HintTypeOnToolError HintType = "on_tool_error"
)

// Hint is one typed in-turn advisory. For `on_tool_result` (the only
// active type):
//
//   - Tools — tool-name match: exact names and/or globs
//     (e.g. "hugr-main:data-*"). Matching is form-insensitive — the
//     `:` / `.` separators and the model-visible `_` form compare
//     equal. Empty → any tool while this skill is loaded.
//   - Match — optional Go regexp over the result text (a runtime
//     error message, or a successful dispatch's raw result body).
//     Empty → any result for the named tools.
//   - Code — optional match on the structured ToolError.Code
//     ("not_found" / "timeout" / …) for a runtime-side error; never
//     matches a successful dispatch (its Code is empty).
//   - Message — the guidance appended inline to the matching result.
type Hint struct {
	Type    HintType `json:"type" yaml:"type"`
	Tools   []string `json:"tools,omitempty" yaml:"tools,omitempty"`
	Match   string   `json:"match,omitempty" yaml:"match,omitempty"`
	Code    string   `json:"code,omitempty" yaml:"code,omitempty"`
	Message string   `json:"message" yaml:"message"`

	// re is the compiled Match, populated at parse (validateHugen).
	// nil when Match is empty (match-any) or before compilation.
	re *regexp.Regexp
}

// canonToolName folds a tool name to a separator-insensitive form so
// the authored canonical spelling ("hugr-main:discovery-search") and
// the model-visible spelling ("hugr-main_discovery-search") compare
// equal. Both `:` and `.` collapse to `_`.
func canonToolName(s string) string {
	return strings.NewReplacer(":", "_", ".", "_").Replace(s)
}

// matchesTool reports whether the hint's Tools globs cover toolName.
// Empty Tools → matches any tool (scope = this skill is loaded).
func (h Hint) matchesTool(toolName string) bool {
	if len(h.Tools) == 0 {
		return true
	}
	cand := canonToolName(toolName)
	for _, pat := range h.Tools {
		if ok, _ := path.Match(canonToolName(pat), cand); ok {
			return true
		}
	}
	return false
}

// MatchToolResult returns the hint's Message when it matches a tool
// result, or "" otherwise. Used by the skill extension's
// [extension.ModelInTurnAdvisor.OnToolResult], which feeds it EVERY
// tool result — runtime error and successful dispatch alike. The
// runtime does not pre-classify error-vs-success; the discriminating is
// here: code is the structured ToolError.Code (empty for a successful
// dispatch) and resultText is the runtime error message OR the raw
// result body. A non-on_tool_result hint never matches. A Code-bearing
// hint only fires on a runtime error (a successful dispatch's empty
// code can't equal a non-empty Code); a regex-only hint fires wherever
// the body matches.
func (h Hint) MatchToolResult(toolName, code, resultText string) string {
	if h.Type != HintTypeOnToolResult {
		return ""
	}
	if !h.matchesTool(toolName) {
		return ""
	}
	if h.Code != "" && !strings.EqualFold(h.Code, code) {
		return ""
	}
	if re := h.compiled(); re != nil {
		if !re.MatchString(resultText) {
			return ""
		}
	}
	return strings.TrimSpace(h.Message)
}

// compiled returns the parse-time-compiled regex, lazily compiling
// Match as a cold-path fallback for Hints constructed outside Parse
// (e.g. tests). nil when Match is empty.
func (h Hint) compiled() *regexp.Regexp {
	if h.re != nil {
		return h.re
	}
	if strings.TrimSpace(h.Match) == "" {
		return nil
	}
	re, err := regexp.Compile(h.Match)
	if err != nil {
		return nil // Parse rejects invalid regex; this is unreachable in prod.
	}
	return re
}

// AllRequires returns the merged transitive-dependency list,
// combining the legacy Requires field with the phase-4 canonical
// RequiresSkills. Order: RequiresSkills first, then any Requires
// entries not already present (de-duped). Caller can iterate the
// result without worrying about which key the manifest used.
func (h HugenMetadata) AllRequires() []string {
	if len(h.RequiresSkills) == 0 {
		return h.Requires
	}
	if len(h.Requires) == 0 {
		return h.RequiresSkills
	}
	seen := make(map[string]struct{}, len(h.RequiresSkills)+len(h.Requires))
	out := make([]string, 0, len(h.RequiresSkills)+len(h.Requires))
	for _, n := range h.RequiresSkills {
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	for _, n := range h.Requires {
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
}

// StuckDetectionPolicy tunes the per-pattern stuck-detection
// heuristics on a session. Zero-value fields fall back to the
// runtime's defaults (repeated_hash=3, tight_density_count=3,
// tight_density_window=2s). Enabled defaults to true — set
// explicitly to false to disable a single pattern at the skill
// level. See phase-4-spec §4.4 + §8.3.
type StuckDetectionPolicy struct {
	RepeatedHash       int    `json:"repeated_hash,omitempty" yaml:"repeated_hash,omitempty"`
	TightDensityCount  int    `json:"tight_density_count,omitempty" yaml:"tight_density_count,omitempty"`
	TightDensityWindow string `json:"tight_density_window,omitempty" yaml:"tight_density_window,omitempty"`
	// Enabled is a tri-state: nil = default (true), &false = off,
	// &true = explicit on. Pointer rather than bool so an absent
	// key isn't conflated with an explicit `enabled: false`.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
}

// IsEnabled returns the effective enable state of stuck detection
// for this skill: true when Enabled is nil (default) or explicitly
// &true; false only when Enabled is &false.
func (p StuckDetectionPolicy) IsEnabled() bool {
	if p.Enabled == nil {
		return true
	}
	return *p.Enabled
}

// AutoloadInTier reports whether the manifest opts into autoload
// for the given tier, ignoring phase-4 conditional gates. Use
// AutoloadEligible when the conditional flags
// (AutoloadWhenRoleCanSpawn / AutoloadWhenParentHasActiveWhiteboard)
// matter — typically at spawn time.
//
// Phase 4.2.2: AutoloadFor must be explicit when Autoload is true
// (parse-time invariant). No empty-defaults — authors declare tier
// placement deliberately.
func (m *Manifest) AutoloadInTier(tier string) bool {
	if !m.Hugen.Autoload {
		return false
	}
	return slices.Contains(m.Hugen.AutoloadFor, tier)
}

// EffectiveTierCompatibility returns the set of tiers where the
// skill is loadable. Falls back to [TierWorker] when the manifest
// omits the field — the default per phase 4.2.2 §3.3.2 (safest
// tier where domain tools are useful; matches every existing
// bundled extension's intent).
func (m *Manifest) EffectiveTierCompatibility() []string {
	if len(m.Hugen.TierCompatibility) > 0 {
		return m.Hugen.TierCompatibility
	}
	return []string{TierWorker}
}

// LoadableInTier reports whether the skill may be loaded into a
// session at the given tier. Consulted by the skill:load gate and
// the per-turn advertise — outside the returned set, the skill is
// invisible from the model's perspective.
func (m *Manifest) LoadableInTier(tier string) bool {
	return slices.Contains(m.EffectiveTierCompatibility(), tier)
}

// AutoloadEligible reports whether the manifest should be
// autoloaded into a session at the given tier. Phase 4.2.2 §3.3.1
// — the phase-4 conditional gates (AutoloadWhenRoleCanSpawn /
// AutoloadWhenParentHasActiveWhiteboard) are gone; tier_compatibility
// + the on_mission_start hook (phase γ) cover the same semantics
// declaratively.
func (m *Manifest) AutoloadEligible(tier string) bool {
	return m.AutoloadInTier(tier)
}

// Tier labels accepted in autoload_for / tier_compatibility entries
// and returned by TierFromDepth. The set is closed: the parser
// rejects any other value.
const (
	TierRoot    = "root"
	TierMission = "mission"
	TierWorker  = "worker"
)

// validTiers is the closed set of tier labels the manifest parser
// accepts in autoload_for / tier_compatibility entries.
var validTiers = map[string]struct{}{
	TierRoot:    {},
	TierMission: {},
	TierWorker:  {},
}

// TierFromDepth maps a session's depth to its tier. depth 0 is
// the user-facing root, depth 1 is the mission root spawns, and
// depth ≥ 2 is a worker (spawned by a mission, or by another
// worker via opt-in can_spawn:true). Phase 4.2.2 §2.
func TierFromDepth(depth int) string {
	switch {
	case depth <= 0:
		return TierRoot
	case depth == 1:
		return TierMission
	default:
		return TierWorker
	}
}

// SubAgentRole is the manifest shape phase-3 validates and
// phase-4 dispatches.
type SubAgentRole struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Tools       []ToolGrant `json:"tools,omitempty"`

	// Prompt is the role's behavioral brief — the domain / mission-
	// specific instructions the runtime renders INTO the spawned
	// subagent's first message, via the universal mission task
	// templates' `[Your role]` slot. Distinct from Description, which
	// stays SHORT (the one-line catalogue text the planner / root read
	// to PICK this role). The universal templates carry only PDCA
	// mechanics; everything role- / domain-specific (which refs to
	// read, query grammar, output discipline) lives here. Empty → the
	// subagent runs on the bare universal template. Phase B34.
	Prompt string `json:"prompt,omitempty" yaml:"prompt,omitempty"`

	// CanSpawn controls whether this role itself may call
	// spawn_subagent (phase-4-spec §3 step 8 + §4.4). Default true:
	// nil means "not specified, use default", &false explicitly
	// disallows further spawning, &true is the redundant-explicit
	// case. Pointer so the default-true semantics survive the
	// missing-key case (a plain bool would default to false).
	CanSpawn *bool `json:"can_spawn,omitempty" yaml:"can_spawn,omitempty"`

	// Intent names the model-router intent this role's child session
	// resolves through (default | cheap | tool_calling | … | any
	// custom intent registered in models.routes). Empty inherits the
	// parent's default intent — operators put fast / cheap roles on
	// `cheap`, deep-reasoning roles on `default`. Phase-4.1d wiring:
	// the spawn flow calls child.SetDefaultIntent(model.Intent(role.Intent))
	// when this field is set.
	Intent string `json:"intent,omitempty" yaml:"intent,omitempty"`

	// Timeout caps this role's per-spawn wall-clock. The mission
	// executor runs the role's wave under a context with this
	// deadline, so a stuck / runaway / never-returning subagent
	// fails the wave (and the planner replans / the mission ends)
	// instead of wedging the executor's wait forever. A Go duration
	// string ("1h", "30m", "20m"); empty inherits the runtime's
	// DefaultWaveTimeout. Validated at parse (fail loud on a bad
	// duration).
	Timeout string `json:"timeout,omitempty" yaml:"timeout,omitempty"`

	// OnClose configures the deterministic pre-teardown turn the
	// runtime fires for this role's worker sessions before
	// emitting SessionTerminated. Phase 4.2.3 ε — gives a narrow
	// "what's worth recording?" prompt with a restricted tool
	// surface so even weak models reliably persist their findings
	// to the notepad. When zero / absent, the runtime falls back
	// to the dispatching skill's mission-level on_close, then to
	// the autoloaded `_worker` base. Per-role override wins.
	OnClose MissionOnClose `json:"on_close,omitempty" yaml:"on_close,omitempty"`

	// AutoloadSkills lists skills the runtime loads on the freshly
	// spawned worker BEFORE its first model turn. The names are
	// resolved through the SkillManager and routed through the
	// child's SessionSkill.Load — same tier check applies (each
	// target must be loadable in the worker's tier). Removes the
	// "every worker calls skill:load(...) before doing anything"
	// ritual when the role's tool surface is known at design time.
	// Per-skill failures log and continue — one bad autoload must
	// not deny the worker its base surface.
	AutoloadSkills []string `json:"autoload_skills,omitempty" yaml:"autoload_skills,omitempty"`

	// Capabilities declares the per-role mission-PDCA capability
	// opt-ins applied by the mission ext at worker spawn time.
	// Empty leaves capability defaults in place (phase-role classes
	// — planner/checker/synthesizer — get plan_context: read; Do
	// roles get it off). Phase F (design 003).
	Capabilities SubAgentCapabilities `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`

	// Compactor is the optional per-role override for the phase-5.2
	// history compactor. Applied to worker sessions spawned for
	// this role. Wins over both the tier default and the
	// dispatching mission's mission-level override. Pointer so
	// absent reads differently from "present with all defaults".
	// See design/004-runtime-post-phase-i/phase-5.2-compactor-spec.md
	// §4.4 — the narrow "let one worker role opt in to compactor
	// while peers stay cheap" lever.
	Compactor *CompactorOverride `json:"compactor,omitempty" yaml:"compactor,omitempty"`
}

// CompactorOverride is the per-skill / per-role override block
// for the phase-5.2 history compactor. Same field surface as the
// operator-level CompactorTier (pkg/config), kept here as a
// separate type so pkg/skill never imports pkg/config — the
// dependency arrow stays config → skill, not the other way. The
// compactor extension reconciles both shapes at resolve time
// (pkg/extension/compactor/resolve.go).
//
// Every field is a pointer so an absent key is distinct from an
// explicit zero. The three-layer resolver only overwrites
// explicitly-set fields.
type CompactorOverride struct {
	Strategy             *string  `json:"strategy,omitempty"               yaml:"strategy,omitempty"`
	WindowSize           *int     `json:"window_size,omitempty"            yaml:"window_size,omitempty"`
	Enabled              *bool    `json:"enabled,omitempty"                yaml:"enabled,omitempty"`
	MaxTurns             *int     `json:"max_turns,omitempty"              yaml:"max_turns,omitempty"`
	MaxTokens            *int     `json:"max_tokens,omitempty"             yaml:"max_tokens,omitempty"`
	PreservedRecentTurns *int     `json:"preserved_recent_turns,omitempty" yaml:"preserved_recent_turns,omitempty"`
	DigestMaxTokens      *int     `json:"digest_max_tokens,omitempty"      yaml:"digest_max_tokens,omitempty"`
	KeptVerbatimMax      *int     `json:"kept_verbatim_max,omitempty"      yaml:"kept_verbatim_max,omitempty"`
	MinTurnGap           *int     `json:"min_turn_gap,omitempty"           yaml:"min_turn_gap,omitempty"`
	LLMTimeoutMs         *int     `json:"llm_timeout_ms,omitempty"         yaml:"llm_timeout_ms,omitempty"`
	LLMIntent            *string  `json:"llm_intent,omitempty"             yaml:"llm_intent,omitempty"`
	TokenBudgetRatio     *float64 `json:"token_budget_ratio,omitempty"     yaml:"token_budget_ratio,omitempty"`
}

// SubAgentCapabilities declares which mission-PDCA surfaces the
// worker session of this role opts into. Each field is a stringly-
// typed access mode (`off` | `read`) so the manifest can express
// "this Do role should see plan_context but not write to it" in
// one place. Empty fields fall through to the runtime's
// role-class default (phase roles default `read`, Do roles default
// `off`). Phase F (design 003).
type SubAgentCapabilities struct {
	// PlanContext gates the [Plan context] section in the worker's
	// first message. Values: "off" (default for Do roles) | "read"
	// (default for phase roles: planner / checker / synthesizer).
	// Write access is implicit: workers populate plan_context via
	// the `memory_summary` field on their handoff regardless of
	// the read setting.
	PlanContext string `json:"plan_context,omitempty" yaml:"plan_context,omitempty"`
}

// CanSpawnEffective resolves SubAgentRole.CanSpawn to the boolean
// the runtime actually checks. Default true — only an explicit
// `can_spawn: false` in the manifest disables further spawning for
// this role.
func (r SubAgentRole) CanSpawnEffective() bool {
	if r.CanSpawn == nil {
		return true
	}
	return *r.CanSpawn
}

// TimeoutDuration parses the role's Timeout string into a duration.
// Empty → (0, nil): no per-role override, the runtime applies its
// DefaultWaveTimeout. A malformed string returns the parse error
// (surfaced at manifest validation).
func (r SubAgentRole) TimeoutDuration() (time.Duration, error) {
	if strings.TrimSpace(r.Timeout) == "" {
		return 0, nil
	}
	return time.ParseDuration(strings.TrimSpace(r.Timeout))
}

// MemoryCategory ports the legacy memory.yaml shape.
type MemoryCategory struct {
	TTL         string `json:"ttl,omitempty"`
	MaxItems    int    `json:"max_items,omitempty"`
	SummariseAt int    `json:"summarise_at,omitempty"`
}

// MissionBlock is the dispatch-eligibility metadata for an
// extension that wants to be selectable as a mission via
// session:spawn_mission. Phase 4.2.2 §6.
//
// Enabled is the gate: only skills with Enabled=true appear in
// root's "Available missions" prompt block and pass spawn_mission's
// catalogue validation. Summary is what root sees per skill;
// Keywords is optional hint material consumed by the same prompt
// builder. OnStart fires synthetically before the mission's first
// model turn so the mission boots with plan/whiteboard already
// in place (phase 4.2.2 §7).
type MissionBlock struct {
	Enabled  bool           `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	Summary  string         `json:"summary,omitempty" yaml:"summary,omitempty"`
	Keywords []string       `json:"keywords,omitempty" yaml:"keywords,omitempty"`
	OnStart  MissionOnStart `json:"on_start,omitempty" yaml:"on_start,omitempty"`
	// OnClose configures the deterministic pre-teardown turn
	// fired for mission-tier sessions before emitting
	// SessionTerminated. Phase 4.2.3 ε. Per-role overrides at
	// SubAgentRole.OnClose win when the closing session is a
	// worker spawned with that role; this field is used for the
	// mission-tier session itself (and as fallback for workers
	// whose role doesn't override).
	OnClose MissionOnClose `json:"on_close,omitempty" yaml:"on_close,omitempty"`

	// Research declares an optional pre-planner stage where a
	// dedicated role gathers user clarifications + scopes the goal
	// before the planner sees the mission. Phase 5.x — B15.
	// Absent block means "no research stage" → runtime spawns the
	// planner directly (backwards-compatible default). When the
	// block names a role, the runtime spawns it before
	// runPlannerLoop, surfaces its findings into the planner's
	// plan_context.research_findings + plan_context.resolved_user_inputs,
	// and gates planner spawn on `done: true`.
	Research *MissionResearchBlock `json:"research,omitempty" yaml:"research,omitempty"`

	// Plan declares the mission's planning configuration —
	// mission-PDCA (design 003) shape. When present, the mission ext
	// treats this skill as a PDCA mission. Supports two shapes:
	// `inline` (manifest-declared waves, used by fixtures + cron task
	// skills) and `role` (LLM-driven planner, used by interactive
	// missions). Plan absent means "not a PDCA mission".
	Plan MissionPlanBlock `json:"plan,omitempty" yaml:"plan,omitempty"`

	// Synthesis declares the role that produces the mission's
	// final answer after the last wave. Phase A — minimal shape
	// (role name only); Phase B may add inline templates.
	Synthesis MissionSynthesisBlock `json:"synthesis,omitempty" yaml:"synthesis,omitempty"`

	// Control declares the verdict-emitting role spawned after
	// every non-planner wave. When set, the runtime auto-routes
	// the planner loop based on the checker's `decision` field
	// (continue / amend / inquire / finish). Absent control falls
	// back to the implicit `continue` path the Phase-B loop uses.
	// Phase C.
	Control MissionControlBlock `json:"control,omitempty" yaml:"control,omitempty"`

	// Capabilities declares which mission-PDCA surfaces are
	// active on the mission session itself (the supervisor tier).
	// Used by skill authors to make implicit defaults explicit —
	// today the listed extensions (notepad, whiteboard,
	// plan_context) are always available, so absent values keep
	// the runtime defaults. Phase F (design 003) lands the
	// declarative schema; future phases can narrow defaults to
	// "off unless declared".
	Capabilities MissionCapabilities `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`

	// AcceptanceCriteria is the optional iter-0 seed of mission
	// acceptance criteria. Each statement is a Go-template string
	// rendered with `.Inputs` (the structured map passed to
	// session:spawn_mission). The runtime mints `ac-1`, `ac-2`, ...
	// rows with origin=`manifest` at mission spawn — the planner
	// reads them under [Mission acceptance criteria] and can keep,
	// rewrite (via ac_update), or drop them via the approval modal.
	//
	// Empty / absent → no manifest seed; the planner is responsible
	// for emitting ≥1 ac_add on iter 1. Phase 5.x — B11 §3.2.2.
	AcceptanceCriteria []string `json:"acceptance_criteria,omitempty" yaml:"acceptance_criteria,omitempty"`

	// InputsSchema is a JSON Schema (draft 2020-12) declaring the
	// shape of the structured `inputs` blob the caller passes to
	// `session:spawn_mission(skill=<this>, inputs=...)`. Rendered
	// into the root's `## Available missions` prompt block so the
	// model knows exactly which keys to pass without guessing.
	// Distinct from `task.inputs_schema` — that one fires at
	// `schedule:create`. Phase 6.1d.
	InputsSchema map[string]any `json:"inputs_schema,omitempty" yaml:"inputs_schema,omitempty"`

	// Stages declares optional lifecycle hooks the runtime fires
	// around mission stages — skill-defined behaviour via MCP tool
	// invocations already wired into the mission session (bash:run,
	// python:run_script, …), no new exec surface. Phase 6.x —
	// research→files. Today only the research stage's before/check
	// hooks are honoured; the schema extends to do/control/synthesis
	// as later phases need them. Absent → no hooks (the default
	// research stage runs without scaffold or gate).
	Stages MissionStages `json:"stages,omitempty" yaml:"stages,omitempty"`
}

// MissionStages groups the per-stage lifecycle-hook declarations.
// Each stage may declare a `before` hook (fired before the stage's
// wave spawns — e.g. scaffold the skill's template files into the
// mission dir) and a `check` hook (fired after the stage produces
// its handoff — a gate that re-prompts the role when the hook
// reports failure). Phase 6.x — research→files.
type MissionStages struct {
	// Research carries the pre-planner research stage's hooks.
	Research MissionStageHooks `json:"research,omitempty" yaml:"research,omitempty"`
}

// MissionStageHooks is the before/check hook pair for one stage.
// Either or both may be nil — an absent hook is a no-op for that
// edge of the stage.
type MissionStageHooks struct {
	Before *MissionStageHook `json:"before,omitempty" yaml:"before,omitempty"`
	Check  *MissionStageHook `json:"check,omitempty"  yaml:"check,omitempty"`
}

// MissionStageHook is one lifecycle hook: an invocation of an MCP
// tool already wired into the mission session. Tool is the
// fully-qualified tool name ("bash:run", "python:run_script", …).
// Args is handed to the tool verbatim except that every string
// value (recursively, inside arrays + nested objects too) is
// Go-template-rendered by the runtime against mission paths
// ({{.MissionDir}}, {{.MissionSkill}}) and runtime state
// ({{.Goal}}, {{.Roles}}, {{.Inputs.key}}) before dispatch.
type MissionStageHook struct {
	Tool string         `json:"tool"            yaml:"tool"`
	Args map[string]any `json:"args,omitempty"  yaml:"args,omitempty"`
}

// Task kind constants — used both by manifest authors (yaml value)
// and by [pkg/extension/scheduler] / Phase 6.1c CronApprovalPolicy
// when branching on the declared shape. Keeping them here keeps
// the spelling authoritative in one place.
const (
	// TaskKindWorker is the MVP shape — a single worker session
	// per fire executing the skill's deterministic body. Default
	// when [TaskBlock.Kind] is empty.
	TaskKindWorker = "worker"

	// TaskKindMission is reserved for adaptive plan-driven tasks
	// (the skill is itself a mission). `schedule:create` guards this
	// kind as "not yet supported" in 6.1b MVP.
	TaskKindMission = "mission"
)

// TaskBlock is the typed projection of `metadata.hugen.task`. When
// `Eligible: true`, the skill is selectable by the `schedule:create`
// tool — operators (or the future `_task_builder` mission) bind a
// recurring schedule to it and TaskManager fires the skill per
// scheduler tick. Phase 6 §0.5.4.
//
// Semantics:
//
//   - `Kind` is the fire shape. MVP supports `worker` only;
//     `mission` is reserved (guarded with a "not yet supported"
//     error by `schedule:create` until mission-shape cron lands).
//   - `InputsSchema` validates the structured `inputs` blob the
//     operator passes at task-create time (separate gate from
//     `mission.inputs_schema`, which only fires at
//     `session:spawn_mission`).
//   - `AllowedToolsDefault` is the recommended per-task tool
//     allow-list the approval modal pre-fills. Operator may
//     edit / extend / shrink before approving; the final list
//     freezes into `tasks.spec.allowed_tools` and the future
//     CronApprovalPolicy enforces it at every dispatch.
//   - `BodyIsTemplate`, when true, opts the skill body into per-fire
//     Go-template rendering with [protocol.FireContext]. Default
//     false — SKILL.md is static.
type TaskBlock struct {
	// Eligible is the master flag. `schedule:create` lists only skills
	// where this is true. Absent / false → skill is invisible to
	// the task surface.
	Eligible bool `json:"eligible,omitempty" yaml:"eligible,omitempty"`

	// Kind is `worker` (default) or `mission` (guarded as "not yet
	// supported" in 6.1b MVP). Empty value is treated as `worker`.
	Kind string `json:"kind,omitempty" yaml:"kind,omitempty"`

	// GoalSummary is the default imperative one-line brief used
	// when the caller omits `goal` at task-create time. Surfaces
	// in liveview + notification subjects. Free-form prose.
	GoalSummary string `json:"goal_summary,omitempty" yaml:"goal_summary,omitempty"`

	// Intent overrides the model-router intent the task ext spawns
	// the recipe child with. Empty falls back to the worker tier's
	// default intent (deps.TierIntents[worker]). Recipes that require
	// stronger reasoning (procedural template substitution, multi-
	// step disambiguation) declare `intent: reasoning`; the router
	// then dispatches the child to whichever model spec is wired to
	// that intent in the operator's config. Unknown intent values
	// log a warn and the child keeps the tier default. Phase 6.1d.
	Intent string `json:"intent,omitempty" yaml:"intent,omitempty"`

	// InputsSchema is a JSON Schema (draft 2020-12) validated
	// against the caller's `inputs` blob at task-create time.
	// Empty / nil → skill accepts any inputs (no schema gate).
	// Distinct from `mission.inputs_schema` — that one fires at
	// `session:spawn_mission`, this one at `schedule:create`.
	InputsSchema map[string]any `json:"inputs_schema,omitempty" yaml:"inputs_schema,omitempty"`

	// AllowedToolsDefault is the recommended tool allow-list the
	// approval modal pre-fills. Empty list = no auto-approved
	// tools; operator must approve every dispatch (or extend the
	// list explicitly).
	AllowedToolsDefault []string `json:"allowed_tools_default,omitempty" yaml:"allowed_tools_default,omitempty"`

	// BodyIsTemplate opts the SKILL.md body into per-fire Go-template
	// rendering with [protocol.FireContext]. Default false; relevant
	// only when the skill body contains `{{ ... }}` actions that
	// depend on the per-fire envelope.
	BodyIsTemplate bool `json:"body_is_template,omitempty" yaml:"body_is_template,omitempty"`
}

// MissionCapabilities lists the mission-tier opt-in toggles.
// Pointer-bool fields preserve the "unset → use default" semantics
// so a deliberate `notepad: false` reads differently from "field
// absent". Phase F (design 003).
type MissionCapabilities struct {
	// Notepad — when set, opt the mission session in (`true`) or
	// out (`false`) of notepad surface. Unset (nil) means "use the
	// runtime default": today notepad is always on for mission
	// sessions; future hardening may flip to off-by-default.
	Notepad *bool `json:"notepad,omitempty" yaml:"notepad,omitempty"`

	// Whiteboard — same shape as Notepad. Unset (nil) keeps the
	// runtime default; today whiteboard is on for mission tier.
	Whiteboard *bool `json:"whiteboard,omitempty" yaml:"whiteboard,omitempty"`

	// PlanContext — same shape as Notepad. Unset (nil) keeps the
	// runtime default; the journal is always active inside the
	// mission ext regardless. Future phases may use this knob to
	// gate plan_context auto-extraction.
	PlanContext *bool `json:"plan_context,omitempty" yaml:"plan_context,omitempty"`
}

// MissionControlBlock names the role the runtime spawns to check
// each wave's output and emit a verdict. v1 — role-only; Phase I
// may add inline `verdict.template` or escalation rules.
type MissionControlBlock struct {
	Role string `json:"role,omitempty" yaml:"role,omitempty"`
}

// MissionResearchBlock declares the pre-planner research stage.
// Phase 5.x — B15. When present, the runtime spawns Role before
// the planner loop, lets it ask the user clarifications via
// `session:inquire`, and surfaces its findings into the planner's
// plan_context so iter-1 plans see scope-resolved inputs.
//
// Presence IS the gate: a skill that declares this block (with a
// Role) always runs the research stage. The researcher decides
// per-turn whether to ask (a `done: false` handoff with
// clarifications) or fast-exit on a clear goal (a `done: true`
// handoff with empty clarifications — one cheap turn, no user
// modal). Skills that never need research simply omit the block.
//
// MaxIterations caps re-fire cycles (when research emits `done:
// false` it gets re-spawned, with prior_answers / prior_comments
// folded into its context). Default 3.
type MissionResearchBlock struct {
	Role          string `json:"role,omitempty" yaml:"role,omitempty"`
	MaxIterations int    `json:"max_iterations,omitempty" yaml:"max_iterations,omitempty"`
}

// MissionPlanBlock is the mission-PDCA `plan:` section. `Role`
// drives LLM-planned missions; `Inline` declares waves directly
// in the manifest (used by fixtures and — Phase 6 — cron task
// skills that ship deterministic pipelines).
//
// A skill is a PDCA mission when either Inline is populated OR
// Role is non-empty. The mission ext picks the dispatch path off
// the first non-empty selector.
type MissionPlanBlock struct {
	// Inline is the manifest-declared wave list: the skill author
	// hardcodes the waves, bypassing the planner LLM. Used by
	// fixtures and deterministic task skills. Nil/empty when not
	// used. Previously called `experimental_inline`; renamed at
	// Phase 6.0 once it became load-bearing for task skills.
	Inline *MissionPlanInline `json:"inline,omitempty" yaml:"inline,omitempty"`

	// Role names the planner sub-agent role declared in the
	// skill's sub_agents block. When non-empty, mission ext drives
	// the mission via the iterative planner loop (Phase B): spawn
	// planner with current plan_context → parse plan handoff →
	// run wave → re-spawn planner. Empty falls back to the inline
	// path above.
	Role string `json:"role,omitempty" yaml:"role,omitempty"`

	// Approval declares when the planner must obtain user approval
	// via session:inquire before its plan can be applied. Defaults
	// (Initial=required, Iteration=initial-only) match spec § Phase
	// B; explicit empty strings are normalised to those defaults
	// at projection time.
	Approval MissionPlanApproval `json:"approval,omitempty" yaml:"approval,omitempty"`

	// MaxWaves caps how many planner-driven iterations the runtime
	// runs before forcing synthesis. Doubles as the approval-loop
	// safety cap (canon § 0.5). Zero falls back to the runtime
	// default (10). Max 50 per safety rail.
	MaxWaves int `json:"max_waves,omitempty" yaml:"max_waves,omitempty"`
}

// MissionPlanApproval declares the approval policy applied to the
// planner's first message + every subsequent iteration. Phase B
// recognises a v1 enum surface; Phase I broadens to
// `when_roadmap_shifted` etc.
type MissionPlanApproval struct {
	// Initial controls the FIRST planner spawn's approval gate:
	//   - "required"  → planner MUST call session:inquire and
	//                   obtain a positive response before handoff.
	//   - "skip"      → no approval inquiry expected; runtime auto-
	//                   accepts the planner's first plan.
	// Empty defaults to "required" per spec § Phase B.
	Initial string `json:"initial,omitempty" yaml:"initial,omitempty"`

	// Iteration controls approval on subsequent planner spawns:
	//   - "always"        → every iteration's plan requires approval.
	//   - "never"         → never re-approve after the first plan.
	//   - "initial-only"  → only the first iteration's plan
	//                       inquires; subsequent ones auto-close.
	// Empty defaults to "initial-only" per spec § Phase B.
	Iteration string `json:"iteration,omitempty" yaml:"iteration,omitempty"`
}

// MissionPlanInline carries a fixed wave sequence for Phase-A
// scenarios. Real PDCA missions emit waves dynamically through a
// planner role; this struct exists so the executor's primitives
// can be exercised end-to-end without an LLM in the loop.
type MissionPlanInline struct {
	Waves []MissionPlanWave `json:"waves,omitempty" yaml:"waves,omitempty"`
}

// MissionPlanWave is one parallel batch of subagent spawns inside
// an inline plan. Mirrors the in-flight Wave AST consumed by Plan
// Executor (pkg/extension/mission.Wave).
type MissionPlanWave struct {
	Label     string                `json:"label" yaml:"label"`
	Subagents []MissionPlanSubagent `json:"subagents,omitempty" yaml:"subagents,omitempty"`
}

// MissionPlanSubagent declares one worker within a wave.
type MissionPlanSubagent struct {
	Name      string   `json:"name" yaml:"name"`
	Skill     string   `json:"skill,omitempty" yaml:"skill,omitempty"`
	Role      string   `json:"role,omitempty" yaml:"role,omitempty"`
	Task      string   `json:"task,omitempty" yaml:"task,omitempty"`
	Inputs    any      `json:"inputs,omitempty" yaml:"inputs,omitempty"`
	DependsOn []string `json:"depends_on,omitempty" yaml:"depends_on,omitempty"`
	// InputsFromResolved, when true, makes the runtime replace this
	// subagent's Inputs with the mission's research-stage
	// ResolvedUserInputs map verbatim. Mutually exclusive with
	// literal Inputs. Used by the universal `_run_task` mission so
	// the recipe spawn carries user-confirmed values without the
	// mission needing to know each recipe's schema. Phase 6.1d.
	InputsFromResolved bool `json:"inputs_from_resolved,omitempty" yaml:"inputs_from_resolved,omitempty"`
}

// MissionSynthesisBlock names the role that produces the
// mission's final answer. Phase A — role name only; absent
// SynthesisBlock means "no synthesis step" (executor closes the
// mission immediately after the last wave).
type MissionSynthesisBlock struct {
	Role string `json:"role,omitempty" yaml:"role,omitempty"`
}

// MissionOnStart describes the per-skill boot sequence the runtime
// fires before the spawned mission's first model turn. All three
// sub-blocks are optional: omit any to skip that step. Templates
// use text/template with a fixed vocabulary (.UserGoal,
// .ParentSkill, .Inputs). Phase 4.2.2 §7.
type MissionOnStart struct {
	Plan         MissionOnStartPlan         `json:"plan,omitempty" yaml:"plan,omitempty"`
	Whiteboard   MissionOnStartWhiteboard   `json:"whiteboard,omitempty" yaml:"whiteboard,omitempty"`
	FirstMessage MissionOnStartFirstMessage `json:"first_message,omitempty" yaml:"first_message,omitempty"`
}

// MissionOnStartPlan declares the plan body the runtime sets on
// the mission via the system-principal plan write path before
// the mission's first turn. BodyTemplate runs through text/template;
// CurrentStep is the literal focus step (no template).
type MissionOnStartPlan struct {
	BodyTemplate string `json:"body_template,omitempty" yaml:"body_template,omitempty"`
	CurrentStep  string `json:"current_step,omitempty" yaml:"current_step,omitempty"`
}

// MissionOnStartWhiteboard toggles a synthetic whiteboard:init
// at mission boot. Only Init is meaningful today (true → init);
// future fields may carry initial categories / retention overrides.
type MissionOnStartWhiteboard struct {
	Init bool `json:"init,omitempty" yaml:"init,omitempty"`
}

// MissionOnStartFirstMessage optionally overrides the mission's
// first user-role message. Template runs through text/template.
// When omitted the runtime uses the bare `goal` string from
// spawn_mission as the first user message.
type MissionOnStartFirstMessage struct {
	Template string `json:"template,omitempty" yaml:"template,omitempty"`
}

// NotepadBlock advertises recommended notepad categories. Lives
// at the top of HugenMetadata (sibling to Mission) — every loaded
// skill can declare which categories the model is encouraged to
// use when calling notepad:append while this skill is loaded.
// Pure recommendation: notepad:append accepts any category
// string; these are listed for retrieval coherence.
type NotepadBlock struct {
	Tags []NotepadTagDecl `json:"tags,omitempty" yaml:"tags,omitempty"`
}

// NotepadTagDecl is one recommended category. Hint is the
// one-line description that goes into the system prompt next to
// the name.
type NotepadTagDecl struct {
	Name string `json:"name" yaml:"name"`
	Hint string `json:"hint,omitempty" yaml:"hint,omitempty"`
}

// MissionOnClose declares per-skill / per-role configuration for
// the deterministic close turn the runtime fires before
// SessionTerminated. Phase 4.2.3 ε — the goal is to give weak
// models a narrow, well-prompted final moment to persist findings
// to the notepad, instead of relying on them to remember to call
// notepad:append during their main task.
//
// All sub-blocks are optional; an entirely empty MissionOnClose
// signals "no close turn" for this scope. The first capability
// to be wired is Notepad, but the block is structured so future
// per-extension close hooks (whiteboard digest, plan summary)
// can land alongside without churn.
type MissionOnClose struct {
	Notepad MissionOnCloseNotepad `json:"notepad,omitempty" yaml:"notepad,omitempty"`
}

// MissionOnCloseNotepad configures the notepad close turn. When
// any field is non-zero the runtime treats the block as opt-in:
// at session teardown time it fires one constrained model turn
// with AllowedTools as the tool-surface filter and Prompt as the
// system-prompt addendum (an extension-provided default kicks
// in when Prompt is empty).
//
//   - Prompt: complete system prompt for the close turn. Empty
//     falls back to the notepad extension's built-in default
//     (generic "before close, record what's worth keeping").
//   - AllowedTools: tool-name allow-list (e.g.
//     ["notepad:append"]) that narrows the snapshot for this
//     turn only. Empty falls back to the extension default
//     (typically just notepad:append) so the model can't drift
//     into another tool category.
//   - MaxTurns: cap on close-turn LLM iterations. Zero falls
//     back to the extension default (2 — one append wave +
//     ack). Independent of the session's regular max_turns
//     budget; the close-turn budget is not counted against the
//     main task.
//   - SkipIfIdle: when true, the runtime skips the close turn
//     entirely if the session emitted no tool calls during its
//     main task. Cheap path for trivial sessions
//     (simple-answerer, /end at root).
//   - Skip: when true, the runtime UNCONDITIONALLY skips the
//     close turn regardless of tool-call count. Recipes whose
//     handoff `memory_summary` is the canonical takeaway use
//     this to avoid a second LLM round-trip for a redundant
//     notepad append (the handoff body is the deliverable).
//     Phase 6.1d.
type MissionOnCloseNotepad struct {
	Prompt       string   `json:"prompt,omitempty" yaml:"prompt,omitempty"`
	AllowedTools []string `json:"allowed_tools,omitempty" yaml:"allowed_tools,omitempty"`
	MaxTurns     int      `json:"max_turns,omitempty" yaml:"max_turns,omitempty"`
	SkipIfIdle   bool     `json:"skip_if_idle,omitempty" yaml:"skip_if_idle,omitempty"`
	Skip         bool     `json:"skip,omitempty" yaml:"skip,omitempty"`
}

// IsZero reports whether the MissionOnCloseNotepad is the
// zero-value (no field set). Callers use it to distinguish
// "manifest said nothing" from "manifest opted in with all
// defaults" (which would be an explicit `notepad: {}` block).
func (n MissionOnCloseNotepad) IsZero() bool {
	return n.Prompt == "" &&
		len(n.AllowedTools) == 0 &&
		n.MaxTurns == 0 &&
		!n.SkipIfIdle &&
		!n.Skip
}

// IsZero on MissionOnClose returns true when none of its
// sub-blocks are set. Used by the CloseTurnLookup resolver to
// short-circuit fallback chains.
func (c MissionOnClose) IsZero() bool {
	return c.Notepad.IsZero()
}

var (
	// agentskills.io: name is [A-Za-z0-9_-]{1,64}.
	nameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

	// Frontmatter delimiter — three or more dashes on a line by
	// themselves. The spec uses exactly three; we accept any count
	// ≥3 to be lenient with typos that round-trip through some
	// editors.
	delimRe = regexp.MustCompile(`(?m)^-{3,}\s*$`)
)

// Parse a SKILL.md (or just-frontmatter) byte slice into a
// Manifest. Returns ErrManifestInvalid (wrapped) when validation
// fails; the caller can errors.Is to detect that class.
func Parse(content []byte) (Manifest, error) {
	var m Manifest

	front, body, err := splitFrontmatter(content)
	if err != nil {
		return m, fmt.Errorf("%w: %v", ErrManifestInvalid, err)
	}
	m.Raw = front
	m.Body = body

	if len(front) == 0 {
		return m, fmt.Errorf("%w: empty frontmatter", ErrManifestInvalid)
	}
	if _, err := yaml.Unmarshal(front, &m, yaml.DecodeOpts{}); err != nil {
		return m, fmt.Errorf("%w: yaml: %v", ErrManifestInvalid, err)
	}

	if err := m.validate(); err != nil {
		return m, fmt.Errorf("%w: %v", ErrManifestInvalid, err)
	}

	hugen, err := extractHugen(m.Metadata)
	if err != nil {
		return m, fmt.Errorf("%w: %v", ErrManifestInvalid, err)
	}
	m.Hugen = hugen

	if err := m.validateHugen(); err != nil {
		// errors.Join so both ErrManifestInvalid and any inner
		// sentinel (e.g. ErrAutoloadReserved) reach errors.Is.
		return m, errors.Join(ErrManifestInvalid, err)
	}
	return m, nil
}

// validateHugen runs the parse-time invariants that depend on the
// typed Hugen projection. Separate from validate() because
// extractHugen runs after the top-level validate; folding these
// into validate would couple ordering. Phase 4.2.2 §3.3.
func (m *Manifest) validateHugen() error {
	for i, t := range m.Hugen.AutoloadFor {
		if _, ok := validTiers[t]; !ok {
			return fmt.Errorf("metadata.hugen.autoload_for[%d] = %q: must be one of [%s,%s,%s]",
				i, t, TierRoot, TierMission, TierWorker)
		}
	}
	for i, t := range m.Hugen.TierCompatibility {
		if _, ok := validTiers[t]; !ok {
			return fmt.Errorf("metadata.hugen.tier_compatibility[%d] = %q: must be one of [%s,%s,%s]",
				i, t, TierRoot, TierMission, TierWorker)
		}
	}

	if m.Hugen.Autoload {
		if !strings.HasPrefix(m.Name, "_") {
			return fmt.Errorf("metadata.hugen.autoload: true is reserved for system skills (name must start with %q, got %q): %w",
				"_", m.Name, ErrAutoloadReserved)
		}
		if len(m.Hugen.AutoloadFor) == 0 {
			return errors.New("metadata.hugen.autoload: true requires explicit metadata.hugen.autoload_for")
		}
	}
	// mission.enabled is permitted on both `_`-prefixed (system)
	// and bare-named (extension) skills. The former covers
	// runtime-bundled universal mission dispatchers (e.g.
	// `_general` — the catch-all fallback mission for tasks that
	// don't fit a specialised skill). The latter covers
	// operator/community-contributed mission skills like
	// `analyst`, `coder`, `writer`. Autoload is the only `_`-only
	// invariant — see the m.Hugen.Autoload check above. Phase 4.2.2
	// §6 (revised).

	// autoload_for ⊆ effective tier_compatibility — a skill cannot
	// auto-load into a tier where skill:load would reject it.
	if len(m.Hugen.AutoloadFor) > 0 {
		compat := m.EffectiveTierCompatibility()
		compatSet := make(map[string]struct{}, len(compat))
		for _, t := range compat {
			compatSet[t] = struct{}{}
		}
		for _, t := range m.Hugen.AutoloadFor {
			if _, ok := compatSet[t]; !ok {
				return fmt.Errorf("metadata.hugen.autoload_for contains %q but tier_compatibility (effective %v) does not — autoload_for must be a subset of tier_compatibility",
					t, compat)
			}
		}
	}

	// Phase F (design 003) — per-role capability access modes.
	// Accepted: empty (defer to runtime default), "off", "read".
	// Unknown values fail-loud so weak-model-authored manifests
	// don't silently fall back to default.
	for i, r := range m.Hugen.SubAgents {
		switch r.Capabilities.PlanContext {
		case "", "off", "read":
		default:
			return fmt.Errorf("metadata.hugen.sub_agents[%d].capabilities.plan_context = %q: must be one of [\"\", \"off\", \"read\"]",
				i, r.Capabilities.PlanContext)
		}
		if _, err := r.TimeoutDuration(); err != nil {
			return fmt.Errorf("metadata.hugen.sub_agents[%d].timeout = %q: invalid duration: %v",
				i, r.Timeout, err)
		}
		// Fail loud on a malformed `prompt` Go template at load time —
		// the runtime renders it best-effort (falls back to the raw
		// string on error), so without this a typo like `{{ .Inputs.x }`
		// would ship the literal braces into the worker's brief
		// silently. Catch it here, in hugen-skill-validate + at startup.
		if strings.Contains(r.Prompt, "{{") {
			if _, err := template.New("prompt").Parse(r.Prompt); err != nil {
				return fmt.Errorf("metadata.hugen.sub_agents[%d] (%s).prompt: invalid Go template: %w",
					i, r.Name, err)
			}
		}
	}

	// Phase 5.x — B15 — mission.research block validation. Fail
	// loud on typos so a misconfigured block doesn't silently
	// disable research stage.
	if r := m.Hugen.Mission.Research; r != nil {
		if strings.TrimSpace(r.Role) == "" {
			return fmt.Errorf("metadata.hugen.mission.research.role is required when the block is present")
		}
		if r.MaxIterations < 0 {
			return fmt.Errorf("metadata.hugen.mission.research.max_iterations = %d: must be >= 0", r.MaxIterations)
		}
	}

	// Phase 6.x — metadata.hugen.hints. Fail loud on a bad regex so
	// a typo surfaces at hugen-skill-validate, not at the cold tool-
	// error path. Compile here and stash the result on the stored
	// Hint so the runtime never recompiles. An unknown Type is
	// tolerated (forward-compat): no message text + no regex compile,
	// the runtime's variation dispatch simply ignores it.
	for i := range m.Hugen.Hints {
		h := &m.Hugen.Hints[i]
		if h.Type == "" {
			return fmt.Errorf("metadata.hugen.hints[%d].type is required", i)
		}
		// Deprecated alias: the former error/result split collapsed into
		// one on_tool_result type that fires on every result. Normalise
		// so the stored Hint carries the canonical type and Match works.
		if h.Type == HintTypeOnToolError {
			h.Type = HintTypeOnToolResult
		}
		if h.Type != HintTypeOnToolResult {
			// Forward-compat: keep the entry, skip type-specific
			// validation; the runtime ignores unknown variations.
			continue
		}
		if strings.TrimSpace(h.Message) == "" {
			return fmt.Errorf("metadata.hugen.hints[%d] (type=%s): message is required", i, h.Type)
		}
		if strings.TrimSpace(h.Match) != "" {
			re, err := regexp.Compile(h.Match)
			if err != nil {
				return fmt.Errorf("metadata.hugen.hints[%d].match: invalid regexp %q: %v", i, h.Match, err)
			}
			h.re = re
		}
	}

	// A task-eligible skill surfaces as a synthetic `task:<name>` tool, so
	// its name must not collide with the static task-provider tools
	// (`task:search` / `task:describe` / `task:execute_task`) — a collision
	// shadows the static tool in the catalogue and makes the recipe
	// permanently mis-dispatched. Reject at the authoring boundary.
	if m.Hugen.Task.Eligible {
		if _, bad := reservedTaskToolNames[m.Name]; bad {
			return fmt.Errorf("a task-eligible skill cannot be named %q — it collides with the built-in task:%s tool; choose another name: %w",
				m.Name, m.Name, ErrReservedTaskName)
		}
	}
	return nil
}

// reservedTaskToolNames are the static task-provider tool short-names a
// task-eligible skill must not take (its synthetic `task:<name>` tool
// would shadow them). Kept in sync with the toolName* consts in
// pkg/extension/task.
var reservedTaskToolNames = map[string]struct{}{
	"search":       {},
	"describe":     {},
	"execute_task": {},
}

// ParseReader is a convenience wrapper around Parse for io.Reader.
func ParseReader(r io.Reader) (Manifest, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: read: %v", ErrManifestInvalid, err)
	}
	return Parse(b)
}

// splitFrontmatter returns (frontmatter-bytes, body-bytes). The
// frontmatter is the YAML between the opening `---` and the
// closing `---`; the body is everything after the closing `---`.
// A document with no closing delimiter is rejected. A document
// with no frontmatter at all returns empty frontmatter and the
// full content as body.
func splitFrontmatter(content []byte) (front, body []byte, err error) {
	// Skip BOM (U+FEFF, encoded as EF BB BF in UTF-8) and leading
	// whitespace before the opening delimiter.
	trimmed := bytes.TrimPrefix(content, []byte{0xEF, 0xBB, 0xBF})
	trimmed = bytes.TrimLeft(trimmed, " \t\r\n")
	if !bytes.HasPrefix(trimmed, []byte("---")) {
		return nil, content, nil
	}
	loc := delimRe.FindAllIndex(trimmed, 2)
	if len(loc) < 2 {
		return nil, nil, errors.New("frontmatter has no closing delimiter")
	}
	frontStart := loc[0][1]
	frontEnd := loc[1][0]
	front = bytes.TrimSpace(trimmed[frontStart:frontEnd])
	body = bytes.TrimLeft(trimmed[loc[1][1]:], "\r\n")
	return front, body, nil
}

func (m *Manifest) validate() error {
	if !nameRe.MatchString(m.Name) {
		return fmt.Errorf("name %q does not match [A-Za-z0-9_-]{1,64}", m.Name)
	}
	if m.Description == "" {
		return errors.New("description is required")
	}
	if len(m.Description) > 1024 {
		return fmt.Errorf("description length %d exceeds 1024 chars", len(m.Description))
	}
	for i, g := range m.AllowedTools {
		if g.Provider == "" {
			return fmt.Errorf("allowed-tools[%d].provider is required", i)
		}
		// Phase 5.1 § η: requires_approval names must match the
		// grant's Tools list verbatim or be the literal '*'.
		// Validation prevents typos that would silently produce a
		// no-op approval gate at runtime.
		if len(g.RequiresApproval) == 0 {
			continue
		}
		owned := map[string]struct{}{}
		for _, t := range g.Tools {
			owned[t] = struct{}{}
		}
		for j, name := range g.RequiresApproval {
			if name == "*" {
				continue
			}
			if _, ok := owned[name]; !ok {
				return fmt.Errorf("allowed-tools[%d].requires_approval[%d] = %q: must appear in the same entry's tools list or be \"*\"",
					i, j, name)
			}
		}
	}
	return nil
}

// extractHugen pulls metadata.hugen.* into a typed HugenMetadata
// by re-marshalling that sub-tree through YAML. Cheaper than
// hand-walking and tolerates any YAML-shape any layer accepts.
func extractHugen(meta map[string]any) (HugenMetadata, error) {
	var out HugenMetadata
	if meta == nil {
		return out, nil
	}
	raw, ok := meta["hugen"]
	if !ok {
		return out, nil
	}
	encoded, err := yaml.Marshal(raw)
	if err != nil {
		return out, fmt.Errorf("metadata.hugen: marshal: %v", err)
	}
	if _, err := yaml.Unmarshal(encoded, &out, yaml.DecodeOpts{}); err != nil {
		return out, fmt.Errorf("metadata.hugen: unmarshal: %v", err)
	}
	return out, nil
}
