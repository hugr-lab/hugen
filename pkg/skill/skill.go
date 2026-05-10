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
// SkillStore.Get: system > local > community > inline > hub.
type Origin int

const (
	OriginSystem Origin = iota
	OriginCommunity
	OriginLocal
	OriginInline
	OriginHub
)

// String returns the URI-style scheme used in logs and audit
// frames: "system://", "community://", etc.
func (o Origin) String() string {
	switch o {
	case OriginSystem:
		return "system"
	case OriginCommunity:
		return "community"
	case OriginLocal:
		return "local"
	case OriginInline:
		return "inline"
	case OriginHub:
		return "hub"
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
