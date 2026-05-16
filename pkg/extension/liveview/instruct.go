package liveview

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/prompts"
)

// PerTurnPrompt implements [extension.Instructor]. Renders an
// "Active sub-agents" block as a per-turn inject when the calling
// session has direct children alive (active or parked). The block
// sits at the very end of the model's prompt (right before the
// last user message) so the live roster reaches the model in its
// high-attention zone, and outside the cached static prefix so a
// fresh roster every turn does not poison provider-side prompt
// caches. Worker-tier sessions return "" — workers are leaves and
// never fan out, so the block has nothing to advertise.
//
// The block exists to nudge the model toward `notify_subagent` /
// `subagent_dismiss` when the user's next message extends a thread
// covered by an existing parked child. Without this surface the
// model has only its history projection to reason from, and
// terminal `subagent_result` rows read as "done" rather than
// "still addressable". Phase 5.2 π.
//
// Sourcing: the per-session sessionView's children cache + childMeta
// map. liveview already folds child status frames into `v.children`
// (last-known status JSON per direct child) and per-spawn metadata
// into `v.childMeta`. Decoding the cached payload gives us
// lifecycle_state + parked_at without any cross-session walk; the
// only locked read is reportMu (same lock the emit path uses).
//
// Empty return — and thus no block in the inject — when:
//   - the session has no direct children;
//   - every cached child status is undecodable (zero rows after
//     filter);
//   - the session is worker-tier.
//
// Returns the rendered Markdown block on success.
func (e *Extension) PerTurnPrompt(ctx context.Context, state extension.SessionState) string {
	if state == nil {
		return ""
	}
	// Worker-tier: nothing to advertise. Skip without locking.
	// Depth ≥ 2 per skill.TierFromDepth.
	if state.Depth() >= 2 {
		return ""
	}
	v := fromState(state)
	if v == nil {
		return ""
	}
	rows := collectChildRows(v)
	if len(rows) == 0 {
		return ""
	}
	return renderActiveSubagentsBlock(state.Prompts(), rows)
}

// childRow is the per-child shape consumed by the active_subagents
// template. Fields are pre-computed so the template stays free of
// time / conditional logic.
type childRow struct {
	ShortID   string
	Label     string // "role" or "role · skill"
	State     string
	Task      string
	ParkedAge string // empty for non-parked rows
	Hint      string
}

// collectChildRows snapshots the liveview cache into a stable list
// of childRow values. Holds reportMu only for the copy; render time
// is outside the lock.
func collectChildRows(v *sessionView) []childRow {
	v.reportMu.Lock()
	if len(v.children) == 0 {
		v.reportMu.Unlock()
		return nil
	}
	// Stable order: by childID. Adapters that scrape the prompt
	// for diffs see a deterministic shape across turns.
	ids := make([]string, 0, len(v.children))
	for id := range v.children {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	rows := make([]childRow, 0, len(ids))
	for _, id := range ids {
		payload := v.children[id]
		meta := v.childMeta[id]
		row, ok := childRowFromPayload(id, payload, meta)
		if !ok {
			continue
		}
		rows = append(rows, row)
	}
	v.reportMu.Unlock()
	return rows
}

// childRowFromPayload decodes the cached liveview status JSON for a
// child and pairs it with the spawn-time metadata. Returns false
// when the payload is undecodable — the block silently drops broken
// rows rather than poisoning the whole prompt.
func childRowFromPayload(id string, payload json.RawMessage, meta childMetaEntry) (childRow, bool) {
	type childStatus struct {
		LifecycleState string    `json:"lifecycle_state,omitempty"`
		ParkedAt       time.Time `json:"parked_at,omitempty"`
	}
	var cs childStatus
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &cs); err != nil {
			return childRow{}, false
		}
	}
	if cs.LifecycleState == "" {
		// No status frame yet — the child just spawned. Surface
		// it as "active" so the model doesn't try to dismiss a
		// fresh sub-agent.
		cs.LifecycleState = "active"
	}
	label := meta.Role
	if label == "" {
		label = "subagent"
	}
	if meta.Skill != "" && meta.Skill != meta.Role {
		label = meta.Role + " · " + meta.Skill
	}
	return childRow{
		ShortID:   shortID(id),
		Label:     label,
		State:     cs.LifecycleState,
		Task:      truncateInline(meta.Task, 120),
		ParkedAge: parkedAge(cs.ParkedAt),
		Hint:      hintForState(cs.LifecycleState),
	}, true
}

// shortID returns the first 8 chars of the session id (or the
// whole id if it's shorter), matching the convention used by the
// TUI's `/mission` modal so adapter + model views align.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	// Session ids start with `ses-` plus 24 hex chars. The first
	// 4 after the prefix is the human-readable distinguisher.
	if len(id) > 4 && id[:4] == "ses-" && len(id) >= 12 {
		return id[4:12]
	}
	return id[:8]
}

// truncateInline caps Task at n bytes for the prompt block. The
// suffix marker matches notepad's convention so the model sees
// "snippet…" rather than a hard cut.
func truncateInline(s string, n int) string {
	if s == "" {
		return "(no task recorded)"
	}
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// parkedAge returns a compact age string ("12s", "1m04s") for a
// parked child; empty for non-parked rows. Bounded to coarse
// resolution — the block re-renders every turn so wall-clock
// freshness comes for free.
func parkedAge(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	d := time.Since(at).Truncate(time.Second)
	if d < 0 {
		return "<1s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	mins := int(d.Minutes())
	secs := int(d.Seconds()) - mins*60
	if secs == 0 {
		return fmt.Sprintf("%dm", mins)
	}
	return fmt.Sprintf("%dm%02ds", mins, secs)
}

// hintForState picks the per-row action hint based on the child's
// lifecycle state. Parked rows get the dismiss/notify call-out;
// active rows get a "leave alone" cue; wait_* rows get the same
// (the child is doing its own thing).
func hintForState(state string) string {
	switch state {
	case "awaiting_dismissal":
		return "parked — `notify_subagent` to re-arm with a follow-up, `subagent_dismiss` to free."
	case "active":
		return "still running — leave alone, or `notify_subagent` if you must redirect."
	case "wait_subagents", "wait_approval", "wait_user_input":
		return "blocked on its own work — do not interrupt unless redirecting the goal."
	case "idle":
		return "idle between turns — typically a recently-resumed root; ignore."
	default:
		return "state unrecognised — leave alone."
	}
}

// renderActiveSubagentsBlock drives the template render with a
// fixed shape. The empty-rows case short-circuits earlier; this
// helper assumes at least one row.
func renderActiveSubagentsBlock(renderer *prompts.Renderer, rows []childRow) string {
	if renderer == nil || len(rows) == 0 {
		return ""
	}
	return renderer.MustRender("liveview/active_subagents", map[string]any{
		"Count": len(rows),
		"Rows":  rows,
	})
}
