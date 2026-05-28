package skill

import (
	"encoding/json"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// Phase-4.1b-pre stage 3: skill state-change events.
//
// The skill extension emits one [protocol.ExtensionFrame] per
// successful Load / Unload through the tool path. Frames are
// CategoryOp — Recovery replays them on restart to rebuild the
// loaded set; visibility hooks (none today) keep them out of the
// model's history.

// Op constants the skill extension carries on every emitted
// ExtensionFrame.
const (
	OpLoad   = "load"
	OpUnload = "unload"
)

// SessionAllowedSkillsKey is the [extension.SessionState] Value key
// a spawner sets to scope which skills a child may load at runtime.
// The value is a `[]string` whitelist. When the key is PRESENT in
// state.Value:
//
//   - `skill:load` rejects any target whose name is not in the
//     whitelist (or in the universal baseline `_system` / `_worker`
//     which autoload regardless).
//   - The `## Available skills` system-prompt catalogue is filtered
//     to entries from the whitelist; an empty whitelist suppresses
//     the catalogue block entirely.
//   - Already-loaded skill bodies still render — the whitelist gates
//     *new* loads only, not skills the spawner pre-loaded via the
//     manifest's `requires_skills`.
//
// When the key is ABSENT (the default for adapter-opened roots and
// mission-ext wave-workers) the session retains full dynamic-load
// flexibility — `skill:load` runs unconstrained and the catalogue
// renders every loadable skill in the manager.
//
// Canonical caller is the task extension's dispatch path (Phase
// 6.1d): a recipe child gets exactly the skills its manifest
// declares — no recursive `task:*` cascades possible because the
// recipe child can't load the category skill that admits the
// synthetic tool surface in the first place.
const SessionAllowedSkillsKey = "session.allowed_skills"

// LoadOpData is the JSON payload of an [OpLoad] frame.
type LoadOpData struct {
	Name string `json:"name"`
}

// UnloadOpData is the JSON payload of an [OpUnload] frame.
type UnloadOpData struct {
	Name string `json:"name"`
}

// emitLoadOp builds and returns a load-op ExtensionFrame for
// sessionID + author. Caller passes the result to
// [extension.SessionState.Emit].
func newLoadFrame(sessionID string, author protocol.ParticipantInfo, name string) (*protocol.ExtensionFrame, error) {
	data, err := json.Marshal(LoadOpData{Name: name})
	if err != nil {
		return nil, err
	}
	return protocol.NewExtensionFrame(sessionID, author, providerName, protocol.CategoryOp, OpLoad, data), nil
}

// newUnloadFrame builds an unload-op ExtensionFrame.
func newUnloadFrame(sessionID string, author protocol.ParticipantInfo, name string) (*protocol.ExtensionFrame, error) {
	data, err := json.Marshal(UnloadOpData{Name: name})
	if err != nil {
		return nil, err
	}
	return protocol.NewExtensionFrame(sessionID, author, providerName, protocol.CategoryOp, OpUnload, data), nil
}
