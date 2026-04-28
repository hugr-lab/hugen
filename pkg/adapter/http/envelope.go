package http

import (
	"encoding/json"
	stdhttp "net/http"
	"time"
)

// OpenSessionRequest is the body of POST /api/v1/sessions.
type OpenSessionRequest struct {
	Metadata map[string]any `json:"metadata,omitempty"`
}

// OpenSessionResponse is the response of POST /api/v1/sessions.
type OpenSessionResponse struct {
	SessionID string    `json:"session_id"`
	Status    string    `json:"status"`
	OpenedAt  time.Time `json:"opened_at"`
}

// SessionListEntry is a single row in ListSessionsResponse.
type SessionListEntry struct {
	SessionID string         `json:"session_id"`
	Status    string         `json:"status"`
	OpenedAt  time.Time      `json:"opened_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// ListSessionsResponse is the body of GET /api/v1/sessions.
type ListSessionsResponse struct {
	Sessions   []SessionListEntry `json:"sessions"`
	NextCursor string             `json:"next_cursor"`
}

// PostFrameRequest is the body of POST /api/v1/sessions/{id}/post.
type PostFrameRequest struct {
	Kind    string          `json:"kind"`
	Author  PostFrameAuthor `json:"author"`
	Payload json.RawMessage `json:"payload"`
}

// PostFrameAuthor mirrors protocol.ParticipantInfo for the API body.
type PostFrameAuthor struct {
	ID    string   `json:"id"`
	Kind  string   `json:"kind"`
	Name  string   `json:"name,omitempty"`
	Roles []string `json:"roles,omitempty"`
}

// PostFrameResponse is the response of POST /api/v1/sessions/{id}/post.
type PostFrameResponse struct {
	FrameID    string    `json:"frame_id"`
	SessionID  string    `json:"session_id"`
	AcceptedAt time.Time `json:"accepted_at"`
}

// CloseSessionRequest is the body of POST /api/v1/sessions/{id}/close.
type CloseSessionRequest struct {
	Reason string `json:"reason,omitempty"`
}

// CloseSessionResponse is the response of POST /api/v1/sessions/{id}/close.
type CloseSessionResponse struct {
	SessionID string    `json:"session_id"`
	Status    string    `json:"status"`
	ClosedAt  time.Time `json:"closed_at"`
}

// ErrorEnvelope is the canonical body of every non-2xx response.
//
//	{"error": {"code": "<code>", "message": "<msg>"}}
type ErrorEnvelope struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody is the inner object of ErrorEnvelope.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeJSON serialises body as JSON with the given status code.
// Headers must not have been written before this call.
func writeJSON(w stdhttp.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

// writeError serialises an ErrorEnvelope with the given status code.
func writeError(w stdhttp.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, ErrorEnvelope{Error: ErrorBody{Code: code, Message: msg}})
}
