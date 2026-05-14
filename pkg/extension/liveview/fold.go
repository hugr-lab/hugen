package liveview

import (
	"context"
	"encoding/json"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// defaultMaxStale caps how long a dirty session can stay silent
// under continuous frame traffic. The debounce timer is reset on
// every frame, so a child emitting every 1.5 s would starve the
// parent's 2 s debounce forever. maxStale guarantees an emit at
// least once per window even if events keep coming. Tuned at 3×
// debounce so bursty paths still coalesce but steady streams keep
// flowing. Overridable per-view (test fixtures shrink it to keep
// suite latency in check; future config could expose it).
const defaultMaxStale = 3 * defaultDebounce

// loop is the observer goroutine's main loop. Pulls frame events
// off the channel, folds them into the session view, and decides
// when to emit a status frame.
//
// Decision logic:
//   - Force-emit (skip debounce timer) on lifecycle-changing
//     events: SessionStatusPayload state transitions, ToolCall,
//     InquiryRequest / InquiryResponse, SubagentStarted /
//     SubagentResult, AgentMessage{Final&&Consolidated},
//     SessionTerminated; child's own liveview/status frame; child
//     SessionTerminated (drops cache entry).
//   - Other events arm a debounce timer (defaultDebounce).
//   - Continuous floods are bounded by maxStale: even if frames
//     keep arriving inside the debounce window, an emit fires
//     once now-lastEmit ≥ maxStale.
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
		dirty    bool
		timer    *time.Timer
		timerC   <-chan time.Time
		lastEmit = time.Now()
	)
	emit := func() {
		v.emitStatus()
		lastEmit = time.Now()
		dirty = false
		if timer != nil {
			timer.Stop()
			timerC = nil
		}
	}
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
			if force || time.Since(lastEmit) >= v.maxStale {
				emit()
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
				emit()
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
		// Force-emit: a new tool call is the most useful "still
		// alive, doing X" signal for adapters. Without this every
		// tool call only arms the debounce timer, and a session
		// running back-to-back tool calls under 2 s apart would
		// be invisible until the burst ends.
		return true
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
	switch fr := f.(type) {
	case *protocol.SessionTerminated:
		// Child died — drop its entry from our projection so the
		// subtree map stops carrying a stale node. Force-emit so
		// the next layer up sees the topology change immediately.
		v.reportMu.Lock()
		delete(v.children, childID)
		v.reportMu.Unlock()
		return true
	case *protocol.ExtensionFrame:
		if fr.Payload.Extension != providerName || fr.Payload.Op != opStatus {
			// Non-liveview child ExtensionFrames (plan / whiteboard /
			// notepad / skill from the child) carry no rolled-up
			// subtree state — the child's own liveview already
			// summarised them. Treat as activity hint only.
			return false
		}
		v.reportMu.Lock()
		if v.children == nil {
			v.children = map[string]json.RawMessage{}
		}
		// Defensive copy of the embedded Data payload — pump may
		// reuse the frame allocation; keep our own slice.
		data := make(json.RawMessage, len(fr.Payload.Data))
		copy(data, fr.Payload.Data)
		v.children[childID] = data
		v.reportMu.Unlock()
		return true
	}
	// Raw child frames (tool_call, reasoning, agent_message
	// chunks, …) are useful as "child is alive, doing something"
	// hints; they arm the debounce timer via dirty=true in loop()
	// but never directly populate our subtree cache — the child's
	// own liveview status frame is the authoritative summary.
	return false
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
		return
	}
	// Debug-level trace so an operator running with
	// HUGEN_LOG_LEVEL=debug sees status frames flowing in
	// real time. The payload is logged verbatim because it's
	// the answer adapters see — grepping for "liveview: status"
	// in agent.log gives the full liveview trail of a session.
	if v.logger != nil {
		toolName := ""
		v.reportMu.Lock()
		if v.lastTool != nil {
			toolName = v.lastTool.Name
		}
		hasInquiry := v.pendingInquiry != nil
		childCount := len(v.children)
		v.reportMu.Unlock()
		v.logger.Debug("liveview: status",
			"session", v.sessionID,
			"depth", v.depth,
			"state", v.lifecycleState,
			"last_tool", toolName,
			"pending_inquiry", hasInquiry,
			"children", childCount,
			"payload", string(data))
	}
}
