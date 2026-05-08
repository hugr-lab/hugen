package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// SessionSummary is a lightweight projection of a session row used
// by adapters to list sessions.
//
// Lives in pkg/session (not pkg/session/manager) because newSession /
// Session.Spawn constructors build SpawnSpec values from the same
// primitives — keeping the shape next to the consumers avoids an
// import cycle between the manager package and pkg/session.
type SessionSummary struct {
	ID        string
	Status    string
	OpenedAt  time.Time
	UpdatedAt time.Time
	Metadata  map[string]any
}

// OpenRequest carries the parameters for the Manager's Open path.
//
// Phase-4 fields:
//
//   - ParentSessionID, SpawnedFromEventID — set by Session.Spawn for
//     sub-agent sessions via newSession(parent, ...); left zero for
//     root sessions opened by adapters. (Depth/SessionType are no
//     longer carried in OpenRequest — newSession derives them from the
//     parent argument.)
type OpenRequest struct {
	OwnerID      string
	Participants []protocol.ParticipantInfo
	// Metadata is persisted verbatim on the session row. Adapters
	// validate size/shape before passing it through; the manager
	// stores it as-is. For sub-agents the manager also writes
	// metadata["depth"] (set to parent.depth+1, immutable) here.
	Metadata map[string]any

	ParentSessionID    string
	SpawnedFromEventID string
}

// SpawnSpec is the input to Session.Spawn. Carries the model-supplied
// fields from session:spawn_subagent (skill, role, task, inputs) plus
// the parent's spawn-event id used for diagnostics.
type SpawnSpec struct {
	Skill   string
	Role    string
	Task    string
	Inputs  any
	EventID string
	// Metadata is merged into the child session row's metadata map
	// after the manager fills in metadata["depth"] / metadata["spawn_role"]
	// / metadata["spawn_skill"]. Caller-supplied keys win on collision.
	Metadata map[string]any
}

// newSessionID returns a fresh ses-<hex> identifier. Used by both
// the root-session Open path (pkg/session/manager) and Session.Spawn
// for child IDs (pkg/session/spawn.go) — kept package-private here
// rather than on the manager package so spawn doesn't need to import
// the supervisor.
func newSessionID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("ses-%d", time.Now().UnixNano())
	}
	return "ses-" + hex.EncodeToString(b[:])
}
