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
