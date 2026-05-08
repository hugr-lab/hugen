package notepad

import (
	"context"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// Compile-time assertion: notepad participates in the Commander
// pipeline so the runtime registers its slash command on every
// session.
var _ extension.Commander = (*Extension)(nil)

// Commands implements [extension.Commander]. Notepad contributes
// `/note <text>` — the human-typed counterpart to the
// notepad:append tool.
func (e *Extension) Commands() []extension.Command {
	return []extension.Command{{
		Name:        "note",
		Description: "save a note to the session notepad: /note <text>",
		Handler:     e.cmdNote,
	}}
}

func (e *Extension) cmdNote(ctx context.Context, state extension.SessionState, env extension.CommandContext, args []string) ([]protocol.Frame, error) {
	sessionID := state.SessionID()
	if len(args) == 0 {
		return []protocol.Frame{
			protocol.NewError(sessionID, env.AgentAuthor, "empty_note",
				"usage: /note <text>", false),
		}, nil
	}
	np := FromState(state)
	if np == nil {
		return []protocol.Frame{
			protocol.NewError(sessionID, env.AgentAuthor, "note_failed",
				"notepad extension not registered on this session", false),
		}, nil
	}
	id, err := np.Append(ctx, env.Author.ID, strings.Join(args, " "))
	if err != nil {
		return []protocol.Frame{
			protocol.NewError(sessionID, env.AgentAuthor, "note_failed", err.Error(), true),
		}, nil
	}
	return []protocol.Frame{
		protocol.NewSystemMarker(sessionID, env.AgentAuthor, "note_added",
			map[string]any{"note_id": id}),
	}, nil
}
