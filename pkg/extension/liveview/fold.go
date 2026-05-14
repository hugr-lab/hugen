package liveview

import (
	"context"
	"encoding/json"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// loop is the observer goroutine's main loop. Pulls frame events
// off the channel, folds them into the session view, and decides
// when to emit a status frame.
//
// Decision logic:
//   - Force-emit (skip debounce timer) on lifecycle-changing
//     events: SessionStatusPayload state transitions,
//     InquiryRequest / InquiryResponse, SubagentStarted /
//     SubagentResult, AgentMessage{Final&&Consolidated},
//     SessionTerminated.
//   - Other events arm a debounce timer (defaultDebounce).
//   - Timer fires → emit if there have been changes since the
//     last emit. Otherwise let the session stay silent (no
//     idle heartbeats).
//
// `state` SessionState is captured at the first frame arrival —
// it is the same handle InitState received; safe to retain
// across the lifetime of the goroutine since the session owns
// the handle.
func (v *sessionView) loop() {
	var (
		dirty  bool
		timer  *time.Timer
		timerC <-chan time.Time
	)
	for {
		select {
		case ev, ok := <-v.ch:
			if !ok {
				if timer != nil {
					timer.Stop()
				}
				return
			}
			force := v.fold(ev)
			dirty = true
			if force {
				v.emitStatus()
				dirty = false
				if timer != nil {
					timer.Stop()
					timerC = nil
				}
				continue
			}
			if timer == nil {
				timer = time.NewTimer(v.debounce)
				timerC = timer.C
			} else if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
				timer.Reset(v.debounce)
			} else {
				timer.Reset(v.debounce)
			}
		case <-timerC:
			timerC = nil
			if dirty {
				v.emitStatus()
				dirty = false
			}
		}
	}
}

// fold applies a frame event to the in-memory projection and
// returns true when the event is "lifecycle-changing" — caller
// uses the flag to skip the debounce timer.
func (v *sessionView) fold(ev frameEvent) bool {
	switch ev.kind {
	case ownFrame:
		return v.foldOwnFrame(ev.frame)
	case childFrame:
		return v.foldChildFrame(ev.childID, ev.frame)
	}
	return false
}

// foldOwnFrame updates the local activity state. Returns true
// on lifecycle-changing frames.
func (v *sessionView) foldOwnFrame(f protocol.Frame) bool {
	v.reportMu.Lock()
	defer v.reportMu.Unlock()
	switch fr := f.(type) {
	case *protocol.SessionStatus:
		if v.lifecycleState != fr.Payload.State {
			v.lifecycleState = fr.Payload.State
			return true
		}
		// Pending inquiry / last tool call may travel embedded
		// in the marker (phase-α enrichment).
		if fr.Payload.PendingInquiry != nil {
			cp := *fr.Payload.PendingInquiry
			v.pendingInquiry = &cp
			return true
		}
		if fr.Payload.LastToolCall != nil {
			cp := *fr.Payload.LastToolCall
			v.lastTool = &cp
		}
	case *protocol.ToolCall:
		v.lastTool = &protocol.ToolCallRef{
			Name:      fr.Payload.Name,
			StartedAt: time.Now().UTC(),
		}
	case *protocol.InquiryRequest:
		v.pendingInquiry = &protocol.PendingInquiryRef{
			RequestID: fr.Payload.RequestID,
			Type:      fr.Payload.Type,
			Question:  fr.Payload.Question,
			StartedAt: time.Now().UTC(),
		}
		return true
	case *protocol.InquiryResponse:
		v.pendingInquiry = nil
		return true
	case *protocol.SubagentStarted:
		// Lifecycle change for THIS session: it has a new child.
		// The child's own state is delivered via its own
		// liveview frame (caught in foldChildFrame).
		return true
	case *protocol.SubagentResult:
		// One of our children terminated.
		return true
	case *protocol.SessionTerminated:
		return true
	}
	return false
}

// foldChildFrame updates the cached child status when a child's
// liveview frame arrives. Returns true to force-emit so the
// next layer up sees the change with minimal latency.
func (v *sessionView) foldChildFrame(childID string, f protocol.Frame) bool {
	ext, ok := f.(*protocol.ExtensionFrame)
	if !ok {
		return false
	}
	if ext.Payload.Extension != providerName || ext.Payload.Op != opStatus {
		// Non-liveview child frames (raw tool_calls, reasoning,
		// child's own ExtensionFrames from plan / whiteboard /
		// notepad / skill) are interesting only as activity
		// hints. We don't fold them into our cache — the child's
		// own liveview already emitted its rolled-up status; we
		// just wait for that frame.
		return false
	}
	v.reportMu.Lock()
	if v.children == nil {
		v.children = map[string]json.RawMessage{}
	}
	// Defensive copy of the embedded Data payload — pump may
	// reuse the frame allocation; keep our own slice.
	data := make(json.RawMessage, len(ext.Payload.Data))
	copy(data, ext.Payload.Data)
	v.children[childID] = data
	v.reportMu.Unlock()
	return true
}

// emitStatus builds the SessionStatus payload and pushes it
// onto the session's outbox (no persist). The payload carries
// own activity, every sibling extension's ReportStatus
// contribution, and the cached children map.
//
// Called from the observer goroutine; uses the SessionState
// captured on the view at InitState.
func (v *sessionView) emitStatus() {
	state := v.state
	if state == nil {
		return
	}
	v.reportMu.Lock()
	payload := map[string]any{
		"session_id": v.sessionID,
		"depth":      v.depth,
	}
	if v.lifecycleState != "" {
		payload["lifecycle_state"] = v.lifecycleState
	}
	if v.lastTool != nil {
		payload["last_tool_call"] = v.lastTool
	}
	if v.pendingInquiry != nil {
		payload["pending_inquiry"] = v.pendingInquiry
	}
	if len(v.children) > 0 {
		kids := make(map[string]json.RawMessage, len(v.children))
		for k, val := range v.children {
			kids[k] = val
		}
		payload["children"] = kids
	}
	v.reportMu.Unlock()

	// Collect sibling StatusReporter contributions.
	exts := state.Extensions()
	if len(exts) > 0 {
		extMap := map[string]json.RawMessage{}
		for _, ext := range exts {
			if ext.Name() == providerName {
				continue // skip self
			}
			r, ok := ext.(extension.StatusReporter)
			if !ok {
				continue
			}
			data := r.ReportStatus(context.Background(), state)
			if len(data) == 0 {
				continue
			}
			extMap[ext.Name()] = data
		}
		if len(extMap) > 0 {
			payload["extensions"] = extMap
		}
	}

	data, err := json.Marshal(payload)
	if err != nil {
		if v.logger != nil {
			v.logger.Warn("liveview: marshal status payload",
				"session", v.sessionID, "err", err)
		}
		return
	}
	frame := protocol.NewExtensionFrame(
		v.sessionID,
		protocol.ParticipantInfo{},
		providerName, protocol.CategoryMarker, opStatus, data,
	)
	if err := state.OutboxOnly(context.Background(), frame); err != nil && v.logger != nil {
		v.logger.Warn("liveview: outbox emit",
			"session", v.sessionID, "err", err)
	}
}
