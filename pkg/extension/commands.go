package extension

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// CommandContext carries the per-invocation participants the runtime
// attaches before dispatching a slash command. Author is the caller
// (typically the user); AgentAuthor is the system speaker used to
// sign system markers and error frames the handler emits.
type CommandContext struct {
	Author      protocol.ParticipantInfo
	AgentAuthor protocol.ParticipantInfo
}

// CommandFn executes a slash command for an extension. It returns
// the frames to emit on the session's outbox; persistence is the
// session loop's job. SessionState exposes the calling session's
// per-extension handles; CommandContext carries per-invocation
// participants. Handlers must remain pure with respect to I/O
// outside of state / context.
type CommandFn func(ctx context.Context, state SessionState, env CommandContext, args []string) ([]protocol.Frame, error)

// Command is one slash command an extension contributes.
//
// Name is the bare command name (no leading slash) and must match
// the runtime's command-name pattern (`^[a-z][a-z0-9_-]*$`).
// Description is the one-line `/help` listing.
type Command struct {
	Name        string
	Description string
	Handler     CommandFn
}

// Commander extensions register slash commands. The runtime walks
// every Commander-implementing extension at boot and merges the
// returned commands into the session-level CommandRegistry. Names
// must not collide with built-ins or with other extensions'
// commands; the runtime fails fast on collision.
//
// Optional capability — extensions whose surface is purely
// LLM-callable tools (no human-typed slash command) skip it.
type Commander interface {
	Commands() []Command
}
