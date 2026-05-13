package skill

import (
	"errors"
	"io/fs"
)

// Skill is a parsed Manifest plus a handle to its body files.
type Skill struct {
	Manifest Manifest
	Origin   Origin
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
//     (`_root`, `_mission`, `_worker`, `_general`, `_planner`,
//     `_skill_builder`, `_system`, `_whiteboard`). Embed-only;
//     no on-disk presence. Owned by the binary; not tunable.
//   - **hub** — admin-delivered extensions (`hugr-data`,
//     `analyst`, `duckdb-data`, `duckdb-docs`, `python-runner`).
//     Today filled from the binary's embedded bundle at boot
//     into `${state}/skills/hub/`. The future will replace that
//     install with a remote Hugr function call against the
//     deployment's Hub, fetching the per-agent-type bundle.
//   - **local** — operator-authored skills under
//     `${state}/skills/local/`, writable via skill:save.
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

	// ErrTierForbidden is returned by skill load when the calling
	// session's tier (resolved from depth) is not in the manifest's
	// tier_compatibility set. Surfaces to the LLM as
	// tool_error{code:"tier_forbidden"} so the model can react with
	// the appropriate alternative (delegate via spawn_*, load
	// elsewhere, etc.). Phase 4.2.2 §3.3.3.
	ErrTierForbidden = errors.New("skill: tier forbidden")

	// ErrSkillExists is returned by SkillStore.Publish when a skill
	// with the same name already exists in the writable backend
	// and PublishOptions.Overwrite is false. The save protocol
	// (`_skill_builder`) instructs the agent to ask the user
	// before retrying with Overwrite=true.
	ErrSkillExists = errors.New("skill: already exists")

	// ErrInvalidPath is returned by skill:save when a relative key
	// in the bundle (references/scripts/assets) escapes the skill
	// directory or contains otherwise unsafe characters. See
	// pkg/skill.CleanRelPath for the validation rules.
	ErrInvalidPath = errors.New("skill: invalid bundle path")
)
