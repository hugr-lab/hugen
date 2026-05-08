package http

import (
	"context"
	"encoding/json"
	"errors"
	stdhttp "net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/session/manager"
)

// allowedKinds enumerates the inbound Frame kinds the API accepts on
// the post endpoint (FR-014). Other kinds are agent-produced or
// internal and are rejected with 400 invalid_kind.
var allowedKinds = map[protocol.Kind]struct{}{
	protocol.KindUserMessage:  {},
	protocol.KindSlashCommand: {},
	protocol.KindCancel:       {},
}

// decodeBody decodes the request body into v after wrapping it with
// http.MaxBytesReader. Returns (decoded, ok); when ok is false the
// caller should not write any further response.
//
//   - Body absent / Content-Length 0 → ok=true, no decode (caller
//     handles defaults).
//   - Oversized body → 413 payload_too_large.
//   - Malformed JSON → 400 invalid_envelope.
func (a *Adapter) decodeBody(w stdhttp.ResponseWriter, r *stdhttp.Request, v any) bool {
	if r.Body == nil || r.ContentLength == 0 {
		return true
	}
	r.Body = stdhttp.MaxBytesReader(w, r.Body, a.maxBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var maxErr *stdhttp.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, stdhttp.StatusRequestEntityTooLarge, "payload_too_large",
				"request body exceeds the configured limit")
			return false
		}
		writeError(w, stdhttp.StatusBadRequest, "invalid_envelope", err.Error())
		return false
	}
	return true
}

// handleOpenSession serves POST /api/v1/sessions.
func (a *Adapter) handleOpenSession(host manager.AdapterHost) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		var req OpenSessionRequest
		if !a.decodeBody(w, r, &req) {
			return
		}
		// metadata size budget: keep it modest (FR-015 scope is
		// loopback; no need for elaborate quotas in phase 2).
		if len(req.Metadata) > 32 {
			writeError(w, stdhttp.StatusBadRequest, "invalid_metadata", "metadata exceeds 32 entries")
			return
		}
		s, openedAt, err := host.OpenSession(r.Context(), session.OpenRequest{Metadata: req.Metadata})
		if err != nil {
			a.routeError(w, err)
			return
		}
		writeJSON(w, stdhttp.StatusCreated, OpenSessionResponse{
			SessionID: s.ID(),
			Status:    session.StatusActive,
			OpenedAt:  openedAt,
		})
	}
}

// handleListSessions serves GET /api/v1/sessions.
func (a *Adapter) handleListSessions(host manager.AdapterHost) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		q := r.URL.Query()
		status := q.Get("status")
		switch status {
		case "", session.StatusActive, session.StatusSuspended, session.StatusClosed:
			// ok
		default:
			writeError(w, stdhttp.StatusBadRequest, "bad_query", "unknown status: "+status)
			return
		}
		if l := q.Get("limit"); l != "" {
			n, err := strconv.Atoi(l)
			if err != nil || n < 1 || n > 1000 {
				writeError(w, stdhttp.StatusBadRequest, "bad_query", "limit out of range")
				return
			}
		}
		summaries, err := host.ListSessions(r.Context(), status)
		if err != nil {
			a.routeError(w, err)
			return
		}
		entries := make([]SessionListEntry, 0, len(summaries))
		for _, s := range summaries {
			entries = append(entries, SessionListEntry{
				SessionID: s.ID,
				Status:    s.Status,
				OpenedAt:  s.OpenedAt,
				UpdatedAt: s.UpdatedAt,
				Metadata:  s.Metadata,
			})
		}
		writeJSON(w, stdhttp.StatusOK, ListSessionsResponse{Sessions: entries})
	}
}

// handlePostFrame serves POST /api/v1/sessions/{id}/post.
//
// The handler reads the body once into a generic map so it can
// reject client-set envelope fields (frame_id, session_id,
// occurred_at) with a precise error code before any state
// mutation, then decodes the same bytes into the typed
// PostFrameRequest. MaxBytesReader caps the body size; oversize
// → 413 payload_too_large.
func (a *Adapter) handlePostFrame(host manager.AdapterHost) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		sessionID := r.PathValue("id")
		if sessionID == "" {
			writeError(w, stdhttp.StatusBadRequest, "invalid_envelope", "missing session id in path")
			return
		}
		var raw map[string]json.RawMessage
		if !a.decodeBody(w, r, &raw) {
			return
		}
		for _, field := range []string{"frame_id", "session_id", "occurred_at"} {
			if _, present := raw[field]; present {
				writeError(w, stdhttp.StatusBadRequest, "client_set_field",
					"client must not set "+field)
				return
			}
		}
		var req PostFrameRequest
		// We already have the parsed map; pull the typed fields
		// from it without round-tripping through json.Marshal.
		if v, ok := raw["kind"]; ok {
			_ = json.Unmarshal(v, &req.Kind)
		}
		if v, ok := raw["author"]; ok {
			if err := json.Unmarshal(v, &req.Author); err != nil {
				writeError(w, stdhttp.StatusBadRequest, "invalid_envelope", "author: "+err.Error())
				return
			}
		}
		if v, ok := raw["payload"]; ok {
			req.Payload = v
		}
		if req.Kind == "" {
			writeError(w, stdhttp.StatusBadRequest, "invalid_envelope", "kind is required")
			return
		}
		kind := protocol.Kind(req.Kind)
		if _, ok := allowedKinds[kind]; !ok {
			writeError(w, stdhttp.StatusBadRequest, "invalid_kind",
				"only user_message, slash_command, cancel are accepted")
			return
		}
		if req.Author.ID == "" || req.Author.Kind == "" {
			writeError(w, stdhttp.StatusBadRequest, "invalid_envelope",
				"author.id and author.kind are required")
			return
		}
		if req.Author.Kind != protocol.ParticipantUser {
			writeError(w, stdhttp.StatusBadRequest, "invalid_envelope",
				"author.kind must be \"user\" for post")
			return
		}

		// Ensure the session is live for Submit. Resume is idempotent
		// for already-live sessions (manager checks the live map).
		if _, err := host.ResumeSession(r.Context(), sessionID); err != nil {
			a.routeError(w, err)
			return
		}

		frame, err := buildFrame(kind, sessionID, req)
		if err != nil {
			writeError(w, stdhttp.StatusBadRequest, "invalid_envelope", err.Error())
			return
		}
		if err := host.Submit(r.Context(), frame); err != nil {
			a.routeError(w, err)
			return
		}
		writeJSON(w, stdhttp.StatusAccepted, PostFrameResponse{
			FrameID:    frame.FrameID(),
			SessionID:  sessionID,
			AcceptedAt: time.Now().UTC(),
		})
	}
}

// handleCloseSession serves POST /api/v1/sessions/{id}/close.
//
// Idempotent (FR-013): closing an already-closed session returns 200
// with the original closed_at when known.
func (a *Adapter) handleCloseSession(host manager.AdapterHost) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		sessionID := r.PathValue("id")
		if sessionID == "" {
			writeError(w, stdhttp.StatusBadRequest, "invalid_envelope", "missing session id in path")
			return
		}
		var body CloseSessionRequest
		if !a.decodeBody(w, r, &body) {
			return
		}
		reason := body.Reason
		if reason == "" {
			reason = "api_close"
		}
		closedAt, err := host.CloseSession(r.Context(), sessionID, reason)
		if err != nil {
			a.routeError(w, err)
			return
		}
		writeJSON(w, stdhttp.StatusOK, CloseSessionResponse{
			SessionID: sessionID,
			Status:    session.StatusClosed,
			ClosedAt:  closedAt,
		})
	}
}

// handleStream serves GET /api/v1/sessions/{id}/stream.
//
// Order of operations is load-bearing — see
// contracts/sse-wire-format.md §"Reconnection cursor": register the
// subscriber on the runtime fan-out BEFORE reading the replay block,
// so any frame produced during the replay window is queued on the
// live channel rather than dropped.
func (a *Adapter) handleStream(host manager.AdapterHost) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		sessionID := r.PathValue("id")
		if sessionID == "" {
			writeError(w, stdhttp.StatusBadRequest, "invalid_envelope", "missing session id in path")
			return
		}
		flusher, ok := w.(stdhttp.Flusher)
		if !ok {
			writeError(w, stdhttp.StatusInternalServerError, "no_flusher",
				"response writer does not support flushing")
			return
		}

		// Resume on demand so Subscribe attaches to a live session.
		if _, err := host.ResumeSession(r.Context(), sessionID); err != nil {
			a.routeError(w, err)
			return
		}

		// Step 1: register subscriber on the per-session bus. The
		// bus owns the upstream Subscribe and applies the 50ms
		// slow-consumer grace per consumer; cleanup tears down the
		// bus when the last connection drops.
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		sub, cleanup, err := a.attachSubscriber(host, sessionID)
		if err != nil {
			a.routeError(w, err)
			return
		}
		defer cleanup()

		// Step 2: load replay AFTER registration so any live frame
		// produced during the replay query lands on the subscriber
		// channel rather than being missed. The dedupe in
		// writeSSE drops live frames that overlap the replay tail.
		lastEventID := r.Header.Get("Last-Event-ID")
		if lastEventID == "" {
			// Browser EventSource cannot set headers; the SPA
			// passes the cursor as a query param on initial open.
			lastEventID = r.URL.Query().Get("last_event_id")
		}
		minSeq, _ := parseLastEventID(lastEventID)
		replay, err := loadReplay(ctx, a.replay, sessionID, minSeq)
		if err != nil {
			a.routeError(w, err)
			return
		}

		// Step 3: handshake. From here on, errors are written as
		// frames on the stream, not as HTTP statuses.
		setSSEHeaders(w)
		w.WriteHeader(stdhttp.StatusOK)
		flusher.Flush()

		a.writeSSE(ctx, w, flusher, sessionID, replay, sub.out)
	}
}

// routeError converts internal sentinels into the HTTP status +
// ErrorEnvelope catalogue per contracts/http-api.md.
func (a *Adapter) routeError(w stdhttp.ResponseWriter, err error) {
	switch {
	case errors.Is(err, session.ErrSessionNotFound):
		writeError(w, stdhttp.StatusNotFound, "session_not_found", err.Error())
	case errors.Is(err, session.ErrSessionClosed):
		writeError(w, stdhttp.StatusConflict, "session_closed", err.Error())
	default:
		// Treat unknown errors as 500 with a generic code; the
		// inner message preserves the actual error for operator
		// debugging via the loopback log.
		a.logger.Error("http: unhandled error", "err", err)
		writeError(w, stdhttp.StatusInternalServerError, "internal", err.Error())
	}
}

// buildFrame constructs the typed Frame from a validated request.
func buildFrame(kind protocol.Kind, sessionID string, req PostFrameRequest) (protocol.Frame, error) {
	author := protocol.ParticipantInfo{
		ID:    req.Author.ID,
		Kind:  req.Author.Kind,
		Name:  req.Author.Name,
		Roles: req.Author.Roles,
	}
	switch kind {
	case protocol.KindUserMessage:
		var p protocol.UserMessagePayload
		if err := unmarshalPayload(req.Payload, &p); err != nil {
			return nil, err
		}
		return protocol.NewUserMessage(sessionID, author, p.Text), nil
	case protocol.KindSlashCommand:
		var p protocol.SlashCommandPayload
		if err := unmarshalPayload(req.Payload, &p); err != nil {
			return nil, err
		}
		if p.Name == "" && p.Raw != "" {
			// Allow callers to send just `raw`; parse name/args.
			parts := strings.Fields(strings.TrimPrefix(p.Raw, "/"))
			if len(parts) > 0 {
				p.Name = parts[0]
				p.Args = parts[1:]
			}
		}
		return protocol.NewSlashCommand(sessionID, author, p.Name, p.Args, p.Raw), nil
	case protocol.KindCancel:
		var p protocol.CancelPayload
		if err := unmarshalPayload(req.Payload, &p); err != nil {
			return nil, err
		}
		return protocol.NewCancel(sessionID, author, p.Reason), nil
	}
	return nil, errors.New("unsupported kind")
}

func unmarshalPayload(raw json.RawMessage, into any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, into)
}
