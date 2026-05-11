package skill

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

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

	Intents   []string                  `json:"intents,omitempty"`
	SubAgents []SubAgentRole            `json:"sub_agents,omitempty"`
	Memory    map[string]MemoryCategory `json:"memory,omitempty"`

	// Mission, when Enabled, declares the skill as a mission
	// dispatcher: root sees its Summary in the "Available
	// missions" prompt block and may pass it to session:spawn_mission.
	// Phase 4.2.2 §6. The block is enforceable only on extensions
	// (non-`_` names) — system skills are runtime primitives, not
	// dispatch targets.
	Mission MissionBlock `json:"mission,omitempty" yaml:"mission,omitempty"`

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

	// TierCompatibility lists the tiers where the skill may be
	// loaded at all, whether via autoload or via explicit
	// skill:load. Outside this set the skill is invisible: it does
	// not appear in skill:tools_catalog.available_in_skills and a
	// direct skill:load surfaces tool_error{code:"tier_forbidden"}.
	// Empty defaults to ["worker"] — the safest tier where domain
	// tools are useful. Invariant: AutoloadFor ⊆ TierCompatibility
	// (a skill cannot auto-load where it would be forbidden
	// manually). Phase 4.2.2 §3.
	TierCompatibility []string `json:"tier_compatibility,omitempty" yaml:"tier_compatibility,omitempty"`
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
// session at the given tier. Consulted by skill:load and
// skill:tools_catalog gates — outside the returned set, the skill
// is invisible from the model's perspective.
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
	Notepad      MissionOnStartNotepad      `json:"notepad,omitempty" yaml:"notepad,omitempty"`
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

// MissionOnStartNotepad advertises recommended categories the
// mission's worker pool tends to use. Phase 4.2.3 — the runtime
// surfaces Tags into the mission's system prompt (Block A) so
// the model uses consistent labels at notepad:append time.
// Pure recommendation: notepad:append accepts any category string;
// these are listed for retrieval coherence.
type MissionOnStartNotepad struct {
	Tags []NotepadTagDecl `json:"tags,omitempty" yaml:"tags,omitempty"`
}

// NotepadTagDecl is one recommended category. Hint is the
// one-line description that goes into the system prompt next to
// the name.
type NotepadTagDecl struct {
	Name string `json:"name" yaml:"name"`
	Hint string `json:"hint,omitempty" yaml:"hint,omitempty"`
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
	if err := yaml.Unmarshal(front, &m); err != nil {
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
	return nil
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
	if err := yaml.Unmarshal(encoded, &out); err != nil {
		return out, fmt.Errorf("metadata.hugen: unmarshal: %v", err)
	}
	return out, nil
}
