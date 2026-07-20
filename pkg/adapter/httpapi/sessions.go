package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
)

const (
	// ownerMetaKey stamps the API caller onto the session row's metadata so the
	// adapter can owner-scope listing/lookup WITHOUT a runtime query change —
	// SessionSummary already carries Metadata (the a2a `a2a_context_id`
	// precedent). Clients cannot set it (stripped from client metadata).
	ownerMetaKey = "httpapi_owner"
	// channelMetaKey / channelValue record which adapter opened the session.
	channelMetaKey = "channel"
	channelValue   = "httpapi"

	maxBodyBytes = 1 << 20 // 1 MiB request-body cap
)

type createSessionRequest struct {
	Name     string         `json:"name,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	// Tier is intentionally NOT accepted from the client — an API-created root
	// is always the root tier (OpenRequest.Tier empty → derived from depth 0).
	// Letting a client pick the tier would mis-select the tier manual / model
	// intent / approval policy for its own root (L3).
}

type createSessionResponse struct {
	SessionID string    `json:"session_id"`
	Status    string    `json:"status"`
	OpenedAt  time.Time `json:"opened_at"`
}

// sessionDTO is the owner-facing view of a session.
type sessionDTO struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	OpenedAt  time.Time `json:"opened_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// LastSeq is the highest event seq — a UI computes unread against a per-chat
	// last-read cursor. 0 when the session has no events.
	LastSeq int `json:"last_seq"`
}

// handleCreateSession opens a root session owned by the verified caller. Opened
// on the lifecycle ctx (the session's Run loop must outlive this request).
func (a *Adapter) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromCtx(r.Context())
	var body createSessionRequest
	if r.Body != nil {
		if err := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			httpError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}
	// Adapter-owned scoping keys win over anything the client sent.
	meta := map[string]any{}
	for k, v := range body.Metadata {
		if k != ownerMetaKey && k != channelMetaKey {
			meta[k] = v
		}
	}
	meta[ownerMetaKey] = user.UserID
	meta[channelMetaKey] = channelValue

	sess, openedAt, err := a.host.OpenSession(a.lifecycleCtx, session.OpenRequest{
		OwnerID:      user.UserID,
		Participants: []protocol.ParticipantInfo{participantFor(user)},
		Metadata:     meta,
		Name:         body.Name,
	})
	if err != nil {
		a.logger.Error("httpapi: open session", "owner", user.UserID, "err", err)
		httpError(w, http.StatusInternalServerError, "open session failed")
		return
	}
	writeJSON(w, http.StatusCreated, createSessionResponse{
		SessionID: sess.ID(),
		Status:    sess.Status(),
		OpenedAt:  openedAt,
	})
}

// handleListSessions returns the caller's own sessions (owner-scoped),
// optionally filtered by ?status=.
func (a *Adapter) handleListSessions(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromCtx(r.Context())
	all, err := a.host.ListSessions(r.Context(), r.URL.Query().Get("status"))
	if err != nil {
		a.logger.Error("httpapi: list sessions", "err", err)
		httpError(w, http.StatusInternalServerError, "list sessions failed")
		return
	}
	out := make([]sessionDTO, 0, len(all))
	for _, s := range all {
		if ownedBy(s, user.UserID) {
			out = append(out, toDTO(s))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetSession returns one owned session; 404 when absent OR not owned
// (don't leak existence).
func (a *Adapter) handleGetSession(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromCtx(r.Context())
	s, ok := a.findOwned(r.Context(), r.PathValue("id"), user.UserID)
	if !ok {
		httpError(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, toDTO(s))
}

// handleDeleteSession closes one owned session (on the lifecycle ctx).
func (a *Adapter) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromCtx(r.Context())
	id := r.PathValue("id")
	if _, ok := a.findOwned(r.Context(), id, user.UserID); !ok {
		httpError(w, http.StatusNotFound, "session not found")
		return
	}
	if _, err := a.host.CloseSession(a.lifecycleCtx, id, "user_close: httpapi"); err != nil {
		a.logger.Error("httpapi: close session", "id", id, "err", err)
		httpError(w, http.StatusInternalServerError, "close session failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// findOwned looks up a session by id that the caller owns. O(n) over the
// agent's sessions — bounded (one container = one agent); a dedicated getter is
// a later optimisation.
func (a *Adapter) findOwned(ctx context.Context, id, userID string) (session.SessionSummary, bool) {
	if id == "" {
		return session.SessionSummary{}, false
	}
	all, err := a.host.ListSessions(ctx, "")
	if err != nil {
		a.logger.Error("httpapi: lookup session", "id", id, "err", err)
		return session.SessionSummary{}, false
	}
	for _, s := range all {
		if s.ID == id && ownedBy(s, userID) {
			return s, true
		}
	}
	return session.SessionSummary{}, false
}

func ownedBy(s session.SessionSummary, userID string) bool {
	if userID == "" {
		return false
	}
	v, _ := s.Metadata[ownerMetaKey].(string)
	return v == userID
}

func participantFor(u VerifiedUser) protocol.ParticipantInfo {
	p := protocol.ParticipantInfo{ID: u.UserID, Kind: protocol.ParticipantUser, Name: u.Name}
	if u.Role != "" {
		p.Roles = []string{u.Role}
	}
	return p
}

func toDTO(s session.SessionSummary) sessionDTO {
	return sessionDTO{ID: s.ID, Status: s.Status, OpenedAt: s.OpenedAt, UpdatedAt: s.UpdatedAt, LastSeq: s.LastSeq}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
