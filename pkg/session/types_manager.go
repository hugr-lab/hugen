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
	// LastSeq is the session's highest event seq (0 when it has no events).
	// Populated by ListSessions; lets a UI compute unread counts against a
	// per-chat last-read cursor. Zero on paths that don't request it.
	LastSeq int
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
	// Mission is the formal goal recorded on the sessions row's
	// `mission` column at OpenSession time. Phase 4.2.3 — spawn
	// paths pass SpawnSpec.Task here so observability queries
	// (hub.agent.db.sessions) and the upcoming Block B "current
	// mission context" header can see the mission's purpose
	// without scanning the event log. Empty for root sessions.
	Mission string
	// Name is the sanitised subagent name. Empty for root sessions;
	// non-empty + unique among the parent's live children for
	// sub-agents. Set by Session.Spawn after sanitisation +
	// collision-suffix resolution. Persisted into row.Metadata so
	// restore can rebuild s.name. Phase 5.2 α (subagent naming).
	Name string
	// Tier is the resolved semantic role label
	// (skill.TierRoot/TierMission/TierWorker) the session reports
	// via Session.Tier(). Empty in OpenRequest means newSession
	// derives the default from depth via skill.TierFromDepth; the
	// caller in Session.Spawn fills this in non-empty when
	// SpawnSpec.Tier asked for an override. Phase 6.1d.
	Tier string
}

// SpawnSpec is the input to Session.Spawn. Carries the model-supplied
// fields from session:spawn_subagent (skill, role, task, inputs) plus
// the parent's spawn-event id used for diagnostics.
type SpawnSpec struct {
	// Name is the model-supplied short identifier for the child.
	// Spawn sanitises it via SanitizeName + collision-suffixes
	// against the parent's live children before persistence.
	// REQUIRED — Spawn rejects empty Name. Phase 5.2 α
	// (subagent naming).
	Name    string
	Skill   string
	Role    string
	Task    string
	Inputs  any
	EventID string
	// Tier overrides the child's semantic role independent of its
	// structural depth. Empty (the default) means Spawn derives the
	// child's tier from childDepth via skill.TierFromDepth — what
	// every legacy caller relied on. Non-empty values must match a
	// skill.Tier* constant; an invalid value is rejected at Spawn.
	// The canonical non-default caller is the task ext, where an
	// ad-hoc recipe child at depth=1 wants worker semantics (leaf
	// executor, not a coordinator).
	Tier string
	// Metadata is merged into the child session row's metadata map
	// after the manager fills in metadata["depth"] / metadata["spawn_role"]
	// / metadata["spawn_skill"]. Caller-supplied keys win on collision.
	Metadata map[string]any
	// RenderMode tags the child's terminal SubagentResult with a
	// projection hint copied into the payload by the parent's pump.
	// Mirrors the values defined in pkg/protocol
	// ([protocol.SubagentRenderSilent] et al.). Empty falls back to
	// the default render. Used by external extensions (mission ext's
	// Plan Executor) that need to suppress history projection of
	// internal workers without reaching into Session internals.
	RenderMode string
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
