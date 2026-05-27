package scheduler

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
	schedstore "github.com/hugr-lab/hugen/pkg/scheduler/store"
)

// Compile-time assertion: scheduler participates in the Commander
// pipeline so the runtime registers its slash command on every
// session.
var _ extension.Commander = (*Extension)(nil)

// Commands implements [extension.Commander]. Scheduler contributes
// `/tasks` — operator-visible list of tasks owned by the calling
// session, projected as a system message in the chat viewport.
// Used for live diagnostics (next planned fire? still active?)
// without having to crack open the DB.
func (e *Extension) Commands() []extension.Command {
	return []extension.Command{{
		Name:        "tasks",
		Description: "list tasks owned by this session: /tasks [active|paused|cancelled]",
		Handler:     e.cmdTasks,
	}}
}

// cmdTasks renders the owner-scoped task catalogue as a
// human-readable system message. Status filter is optional —
// `/tasks` shows everything; `/tasks active` narrows.
func (e *Extension) cmdTasks(ctx context.Context, state extension.SessionState, env extension.CommandContext, args []string) ([]protocol.Frame, error) {
	sessionID := state.SessionID()
	if e.store == nil {
		return []protocol.Frame{
			protocol.NewError(sessionID, env.AgentAuthor, "scheduler_unavailable",
				"TaskStore is not wired on this runtime", false),
		}, nil
	}

	// Owner = root of this session's tree (task:create anchors on
	// the same walk, so this matches the persisted owner_session_id).
	owner := rootOf(state)
	opts := schedstore.ListTasksOpts{}
	if len(args) > 0 {
		opts.Status = strings.TrimSpace(args[0])
	}
	rows, err := e.store.ListTasksBySession(ctx, owner.SessionID(), opts)
	if err != nil {
		return []protocol.Frame{
			protocol.NewError(sessionID, env.AgentAuthor, "tasks_list_failed", err.Error(), true),
		}, nil
	}
	if len(rows) == 0 {
		filter := "any status"
		if opts.Status != "" {
			filter = "status=" + opts.Status
		}
		return []protocol.Frame{
			protocol.NewSystemMessage(sessionID, env.AgentAuthor, "tasks_list",
				fmt.Sprintf("No tasks owned by this session (filter: %s).", filter)),
		}, nil
	}
	body := renderTaskList(ctx, e.store, rows)
	return []protocol.Frame{
		protocol.NewSystemMessage(sessionID, env.AgentAuthor, "tasks_list", body),
	}, nil
}

// renderTaskList formats the rows into a compact multi-line
// table-ish projection suitable for in-chat display. We probe
// LatestPlannedFire per row so the operator sees the next fire
// instant without a separate query — this is the same N+1 pattern
// as the task:list tool surface.
func renderTaskList(ctx context.Context, store schedstore.TaskStore, rows []schedstore.TaskRow) string {
	// Stable order: active first, then paused, then terminal;
	// alphabetical by name within group.
	statusRank := map[string]int{
		schedstore.StatusActive:    0,
		schedstore.StatusPaused:    1,
		schedstore.StatusCancelled: 2,
		schedstore.StatusCompleted: 3,
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if a, b := statusRank[rows[i].Status], statusRank[rows[j].Status]; a != b {
			return a < b
		}
		return rows[i].Spec.Name < rows[j].Spec.Name
	})

	var b strings.Builder
	fmt.Fprintf(&b, "Tasks owned by this root session (%d):\n", len(rows))
	for i, row := range rows {
		next := ""
		if planned, err := store.LatestPlannedFire(ctx, row.ID); err == nil && planned != nil {
			next = planned.PlannedAt.UTC().Format(time.RFC3339)
		}
		name := row.Spec.Name
		if name == "" {
			name = "(unnamed)"
		}
		fmt.Fprintf(&b, "%d. %s — %s/%s, status=%s",
			i+1, name, row.Kind, row.ScheduleKind, row.Status)
		if row.Spec.ScheduleSpec != "" {
			fmt.Fprintf(&b, " spec=%q", row.Spec.ScheduleSpec)
		}
		if next != "" {
			fmt.Fprintf(&b, " next=%s", next)
		}
		if row.PauseReason != "" {
			fmt.Fprintf(&b, " paused_reason=%s", row.PauseReason)
		}
		fmt.Fprintf(&b, " id=%s\n", row.ID)
	}
	return strings.TrimRight(b.String(), "\n")
}
