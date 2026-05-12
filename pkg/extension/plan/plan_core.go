// Package plan implements the plan session extension: the
// per-session [Plan] projection (Project / Apply / Render), the
// [SessionPlan] handle that wraps the projection + a mutex behind
// Set / Comment / Show / Clear, and the [Extension] wrapper
// exposing the four plan tools and the [extension.StateInitializer]
// / [extension.Recovery] / [extension.Advertiser] hooks the runtime
// dispatches against.
//
// Phase-4 spec §6 + contracts/tools-plan.md + data-model.md §3.1
// govern the projection contract.
package plan

import (
	"time"

	"github.com/hugr-lab/hugen/pkg/prompts"
)

// Caps applied during projection. Events themselves are never
// truncated — only the in-memory view is bounded so a rogue model
// can't blow up the system prompt or the comment log.
const (
	// MaxBodySize is the hard cap on the plan body in bytes
	// (post-truncation). Body that overflows is cut and a marker
	// appended so the model sees the truncation explicitly.
	MaxBodySize = 8192

	// MaxCommentSize is the hard cap on each comment in bytes.
	// Same truncation behaviour as the body.
	MaxCommentSize = 2048

	// MaxComments is the FIFO retention cap on comments in the
	// projection. Older comments fall out of view but stay in events.
	MaxComments = 30

	// TruncationMarker is appended in place of overflow bytes when
	// MaxBodySize / MaxCommentSize are exceeded.
	TruncationMarker = "\n[…truncated]"
)

// Plan is the in-memory projection of a session's plan extension_frame events.
// Active=false means "no plan in projection" — either the events
// log has none yet or the most recent boundary op is "clear". When
// Active is false, every other field is zero by definition.
type Plan struct {
	Active      bool
	Text        string
	CurrentStep string
	Comments    []Comment
	SetAt       time.Time
	UpdatedAt   time.Time
}

// Comment is one progress entry. CurrentStep mirrors the pointer at
// the time the comment was written so a reader replaying the log
// can see how the plan's focus moved over time.
type Comment struct {
	At          time.Time
	CurrentStep string
	Text        string
}

// ProjectEvent is the input shape Project / Apply consume — the
// session package converts a plan extension_frame EventRow into
// this. Time is the row's CreatedAt; Op mirrors
// ExtensionFramePayload.Op; Text / CurrentStep are decoded from
// ExtensionFramePayload.Data.
type ProjectEvent struct {
	At          time.Time
	Op          string // "set" | "comment" | "clear"
	Text        string
	CurrentStep string
}

// OpData is the JSON-encoded payload that rides
// ExtensionFramePayload.Data for plan ops. Set / comment carry both
// fields; clear emits an empty object.
type OpData struct {
	Text        string `json:"text,omitempty"`
	CurrentStep string `json:"current_step,omitempty"`
}

const (
	OpSet     = "set"
	OpComment = "comment"
	OpClear   = "clear"
)

// Project replays events into a fresh Plan. Walks events forward,
// finds the latest set/clear boundary, and accumulates comments
// after a "set" boundary. Comments past MaxComments are dropped
// from the projection (FIFO eviction); the underlying events are
// never deleted.
//
// Empty / clear-terminated histories yield Plan{Active: false}.
func Project(events []ProjectEvent) Plan {
	boundary := -1
	for i, ev := range events {
		if ev.Op == OpSet || ev.Op == OpClear {
			boundary = i
		}
	}
	if boundary < 0 {
		return Plan{}
	}
	if events[boundary].Op == OpClear {
		return Plan{}
	}
	set := events[boundary]
	p := Plan{
		Active:      true,
		Text:        capText(set.Text, MaxBodySize),
		CurrentStep: set.CurrentStep,
		SetAt:       set.At,
		UpdatedAt:   set.At,
	}
	for i := boundary + 1; i < len(events); i++ {
		ev := events[i]
		if ev.Op != OpComment {
			continue
		}
		p = applyComment(p, ev)
	}
	return p
}

// Apply is the pure projection step for one new event. Equivalent
// to Project(events ∥ ev) when called against a Plan derived from
// `events`. Used by tool handlers to update the in-memory cache
// after persisting the corresponding plan extension_frame event.
func Apply(p Plan, ev ProjectEvent) Plan {
	switch ev.Op {
	case OpSet:
		return Plan{
			Active:      true,
			Text:        capText(ev.Text, MaxBodySize),
			CurrentStep: ev.CurrentStep,
			SetAt:       ev.At,
			UpdatedAt:   ev.At,
		}
	case OpClear:
		return Plan{}
	case OpComment:
		if !p.Active {
			// The tool handler should have refused with
			// no_active_plan; if we somehow get here the safe
			// thing is to leave the projection unchanged.
			return p
		}
		return applyComment(p, ev)
	}
	return p
}

// applyComment is the shared comment-append path used by Project's
// inner loop and Apply's "comment" case. Returns a fresh Plan with
// Comments append-then-FIFO-evicted; never mutates the input slice.
func applyComment(p Plan, ev ProjectEvent) Plan {
	out := p
	cmt := Comment{
		At:          ev.At,
		CurrentStep: ev.CurrentStep,
		Text:        capText(ev.Text, MaxCommentSize),
	}
	cs := make([]Comment, 0, len(p.Comments)+1)
	cs = append(cs, p.Comments...)
	cs = append(cs, cmt)
	if len(cs) > MaxComments {
		cs = cs[len(cs)-MaxComments:]
	}
	out.Comments = cs
	if ev.CurrentStep != "" {
		out.CurrentStep = ev.CurrentStep
	}
	out.UpdatedAt = ev.At
	return out
}

// Render formats the active plan as a system-prompt block. Returns
// "" when the plan is inactive — callers can drop the block on a
// clean nil-empty test. Comments are NOT rendered: the model
// retrieves them on demand via plan:show.
//
// Layout (per contracts/tools-plan.md "Prompt-rendering contract"):
//
//	## Active plan
//	Current focus: <current_step>
//
//	<body>
//
// The "Current focus" line is omitted when CurrentStep is empty.
func Render(renderer *prompts.Renderer, p Plan) string {
	if !p.Active {
		return ""
	}
	return renderer.MustRender(
		"plan/snapshot_render",
		map[string]any{
			"CurrentStep": p.CurrentStep,
			"Body":        p.Text,
		},
	)
}

// capText caps a free-form string at max bytes, appending
// TruncationMarker on overrun. Marker length is included in the
// budget so the result is always ≤ max.
func capText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= len(TruncationMarker) {
		return s[:max]
	}
	cut := max - len(TruncationMarker)
	return s[:cut] + TruncationMarker
}
