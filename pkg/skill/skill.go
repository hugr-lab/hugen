package skill

import (
	"errors"
	"io/fs"
)

// skill_log event kinds (db-2). The usage-driven bandit reads these:
//   - SkillLogShown — the skill was surfaced in the advertised catalogue
//     (the impression / denominator).
//   - SkillLogUsed — the model loaded the skill (a load IS the use signal;
//     the conversion / reward).
const (
	SkillLogShown = "shown"
	SkillLogUsed  = "used"
)

// Skill is a parsed Manifest plus a handle to its body files.
type Skill struct {
	Manifest Manifest
	Origin   Origin
	// ID is the dynamic-backend index id (`skl-<hex>`) when this skill
	// is DB-indexed (authored / hub). Empty for system / inline skills
	// that have no index row — usage logging (skill_log) skips those,
	// since skill_log.skill_id is an FK into the skills table.
	ID string
	// FS is rooted at the skill's own directory. Empty for
	// inline skills (Manifest is the entire content).
	FS fs.FS
	// Root is the absolute filesystem path of the skill directory
	// when it lives on disk (system / local / community backends).
	// Empty for inline / hub backends. Used by tools that need
	// real OS paths (e.g. skill:files surfacing absolute
	// paths so bash.read_file / python.run_script can consume
	// them directly).
	Root string
}

// Origin tags where a skill came from. Shadowing order at
// SkillStore.Get: system > hub > local > inline.
//
// Three production sources:
//
//   - **system** — agent-core skills bundled in the binary
//     (`_root`, `_mission_worker`, `_worker`, `_task_builder`,
//     `_skill_builder`, `_system`, `_admin`). Embed-only; no
//     on-disk presence. Owned by the binary; not tunable.
//   - **hub** — admin-delivered extensions (`hugr-data`,
//     `analyst`, `duckdb-data`, `duckdb-docs`, `python-runner`).
//     Today filled from the binary's embedded bundle at boot
//     into `${state}/skills/hub/`. The future will replace that
//     install with a remote Hugr function call against the
//     deployment's Hub, fetching the per-agent-type bundle.
//   - **local** — operator-authored skills under
//     `${state}/skills/local/`, writable via skill:save.
//   - **dynamic** — Phase 6.2.db: the DB-indexed writable user
//     source. On-disk bundles (content) + a `skills` DB row
//     (discovery index: denormalised metadata + semantic
//     description vector + usage log). Consolidates the plain
//     `local` dirBackend — when a querier is wired the runtime
//     builds `dynamic` in local's slot; without one (tests) it
//     falls back to the plain `local` dirBackend.
//
// `inline` is the in-memory channel used by tests and the
// skill:save tool while a session keeps a freshly-authored
// skill before it is written to local.
type Origin int

const (
	OriginSystem Origin = iota
	OriginHub
	OriginLocal
	OriginInline
	// OriginDynamic is the DB-indexed writable user source
	// (Phase 6.2.db). Occupies the same priority slot as
	// OriginLocal — it replaces the plain local dirBackend when a
	// querier is available.
	OriginDynamic
)

// String returns the URI-style scheme used in logs and audit
// frames: "system://", "hub://", etc.
func (o Origin) String() string {
	switch o {
	case OriginSystem:
		return "system"
	case OriginHub:
		return "hub"
	case OriginLocal:
		return "local"
	case OriginInline:
		return "inline"
	case OriginDynamic:
		return "dynamic"
	default:
		return "unknown"
	}
}

// Errors. Sentinel values, errors.Is-comparable.
var (
	// ErrManifestInvalid wraps every Parse-time failure.
	ErrManifestInvalid = errors.New("skill: manifest invalid")

	// ErrSkillNotFound is returned by SkillStore.Get when no
	// backend has the named skill.
	ErrSkillNotFound = errors.New("skill: not found")

	// ErrSkillCycle is returned by SkillManager.Load when the
	// metadata.hugen.requires closure forms a cycle.
	ErrSkillCycle = errors.New("skill: dependency cycle")

	// ErrUnresolvedToolGrant is returned by SkillManager.Load
	// when a granted (provider, tool) pair has no registered
	// provider.
	ErrUnresolvedToolGrant = errors.New("skill: unresolved tool grant")

	// ErrUnsupportedBackend is returned by SkillStore.Publish
	// when the targeted backend is read-only (system, community,
	// hub, inline).
	ErrUnsupportedBackend = errors.New("skill: backend does not support operation")

	// ErrAutoloadReserved is returned by skill:save when a manifest
	// sets metadata.hugen.autoload — autoload is reserved for
	// system / admin skills compiled into the binary, and local
	// skills authored via skill:save must load on demand. See
	// design/002-runtime-canonical/phase-4.2-spec.md §3.2.2.
	ErrAutoloadReserved = errors.New("skill: autoload is reserved for system / admin skills")

	// ErrReservedTaskName is returned by manifest validation when a
	// task-eligible skill takes a name that collides with a static
	// task-provider tool (search / describe / execute_task) — its
	// synthetic task:<name> tool would shadow the built-in.
	ErrReservedTaskName = errors.New("skill: task-eligible skill name collides with a built-in task tool")

	// ErrTierForbidden is returned by skill load when the calling
	// session's tier (resolved from depth) is not in the manifest's
	// tier_compatibility set. Surfaces to the LLM as
	// tool_error{code:"tier_forbidden"} so the model can react with
	// the appropriate alternative (delegate via spawn_*, load
	// elsewhere, etc.). Phase 4.2.2 §3.3.3.
	ErrTierForbidden = errors.New("skill: tier forbidden")

	// ErrSkillExists is returned by SkillStore.Publish when a skill
	// with the same name already exists in the writable backend
	// and PublishOptions.Overwrite is false. The authoring flow
	// (`_task_builder`'s registrar) requires explicit user
	// authorisation before retrying with Overwrite=true.
	ErrSkillExists = errors.New("skill: already exists")

	// ErrInvalidPath is returned by skill:save when a relative key
	// in the bundle (references/scripts/assets) escapes the skill
	// directory or contains otherwise unsafe characters. See
	// pkg/skill.CleanRelPath for the validation rules.
	ErrInvalidPath = errors.New("skill: invalid bundle path")
)
