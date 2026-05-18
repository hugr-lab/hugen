package session

import (
	"context"
	"errors"
	"strings"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// ErrNotifyEmptyTarget is returned by NotifyChild when target is empty.
var ErrNotifyEmptyTarget = errors.New("session: notify: target is required")

// ErrNotifyEmptyContent is returned by NotifyChild when content is empty.
var ErrNotifyEmptyContent = errors.New("session: notify: content is required")

// NotifyChild routes a parent-note frame to a direct child resolved
// by name or session_id. Mirrors the model-callable
// `session:notify_subagent` tool but exposed as a Session method so
// adapter-side slash dispatchers (e.g. console / TUI `/mission
// <name> <directive>`) can deliver the note without a model
// round-trip.
//
// Return values:
//   - (resolvedID, true, nil) — frame submitted; the child observes
//     it via its wait_subagents parent-note path.
//   - ("", false, ErrNotifyEmptyTarget|ErrNotifyEmptyContent) — bad
//     args from the adapter.
//   - ("", false, nil) — target did not resolve to a live direct
//     child (typo or already-terminated). Callers surface this as a
//     usage error.
//   - (resolvedID, false, ctx.Err()) — caller ctx fired while
//     awaiting Submit settle.
func (s *Session) NotifyChild(ctx context.Context, target, content string) (string, bool, error) {
	if strings.TrimSpace(target) == "" {
		return "", false, ErrNotifyEmptyTarget
	}
	if strings.TrimSpace(content) == "" {
		return "", false, ErrNotifyEmptyContent
	}
	if s.IsClosed() {
		return "", false, nil
	}
	resolvedID, child, ok := s.resolveChildTarget(target)
	if !ok || child == nil || child.IsClosed() {
		return "", false, nil
	}
	frame := protocol.NewSystemMessage(child.ID(), s.agent.Participant(),
		protocol.SystemMessageParentNote, content)
	frame.BaseFrame.FromSession = s.id
	settled := child.Submit(ctx, frame)
	select {
	case <-settled:
	case <-ctx.Done():
		return resolvedID, false, ctx.Err()
	}
	if child.IsClosed() {
		return resolvedID, false, nil
	}
	return resolvedID, true, nil
}

// ChildSnapshot is a read-only projection of one live direct child,
// used by adapter-side listing handlers (e.g. `/mission` no-args
// listing in console). Stable contract — the runtime owns the live
// child Session; callers see only the fields they need to render.
type ChildSnapshot struct {
	SessionID string
	Name      string
	Depth     int
}

// SnapshotChildren returns one ChildSnapshot per live direct child
// in name-sort order. Closed children are excluded. Safe for
// concurrent use — acquires childMu for the snapshot.
func (s *Session) SnapshotChildren() []ChildSnapshot {
	s.childMu.Lock()
	defer s.childMu.Unlock()
	out := make([]ChildSnapshot, 0, len(s.children))
	for id, c := range s.children {
		if c == nil || c.IsClosed() {
			continue
		}
		out = append(out, ChildSnapshot{
			SessionID: id,
			Name:      c.name,
			Depth:     c.depth,
		})
	}
	// Name-sort with session_id as tiebreaker for deterministic
	// listing output.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			if out[j].Name < out[j-1].Name ||
				(out[j].Name == out[j-1].Name && out[j].SessionID < out[j-1].SessionID) {
				out[j], out[j-1] = out[j-1], out[j]
				continue
			}
			break
		}
	}
	return out
}
