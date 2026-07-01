package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// The write path (H4): a caller drives its session by submitting inbound
// frames. Each is a discrete POST; the reply (and any inquiry) surfaces on the
// SSE stream (H5). Every handler ownership-checks {id} first, then submits a
// frame authored by the verified user.

type sendMessageRequest struct {
	Text string `json:"text"`
}

// inquiryResponseRequest — the client answers with request_id + the answer only.
// It does NOT echo which session raised the inquiry; the runtime routes by
// request_id (dispatchInquiryResponse).
type inquiryResponseRequest struct {
	RequestID        string                          `json:"request_id"`
	Approved         *bool                           `json:"approved,omitempty"`
	Response         string                          `json:"response,omitempty"`
	Reason           string                          `json:"reason,omitempty"`
	Answers          map[string]protocol.AnswerEntry `json:"answers,omitempty"`
	AutoApproveTools bool                            `json:"auto_approve_tools,omitempty"`
}

type cancelRequest struct {
	Reason  string `json:"reason,omitempty"`
	Cascade bool   `json:"cascade,omitempty"`
}

// handleSendMessage submits a user message (Submit UserMessage). The turn runs
// on the session loop; the reply arrives on the stream — so this returns 202.
func (a *Adapter) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	user, id, ok := a.ownedRequest(w, r)
	if !ok {
		return
	}
	var body sendMessageRequest
	if err := decodeBody(r, &body); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		httpError(w, http.StatusBadRequest, "text required")
		return
	}
	a.submit(w, r, protocol.NewUserMessage(id, participantFor(user), body.Text))
}

// handleInquiryResponse answers a pending HITL inquiry (Submit InquiryResponse).
func (a *Adapter) handleInquiryResponse(w http.ResponseWriter, r *http.Request) {
	user, id, ok := a.ownedRequest(w, r)
	if !ok {
		return
	}
	var body inquiryResponseRequest
	if err := decodeBody(r, &body); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.RequestID == "" {
		httpError(w, http.StatusBadRequest, "request_id required")
		return
	}
	a.submit(w, r, protocol.NewInquiryResponse(id, participantFor(user), protocol.InquiryResponsePayload{
		RequestID:        body.RequestID,
		Approved:         body.Approved,
		Response:         body.Response,
		Reason:           body.Reason,
		Answers:          body.Answers,
		AutoApproveTools: body.AutoApproveTools,
	}))
}

// handleCancel aborts the session's in-flight turn (Submit Cancel). cascade
// also terminates the sub-agent subtree.
func (a *Adapter) handleCancel(w http.ResponseWriter, r *http.Request) {
	user, id, ok := a.ownedRequest(w, r)
	if !ok {
		return
	}
	var body cancelRequest
	if err := decodeBody(r, &body); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	reason := body.Reason
	if reason == "" {
		reason = "user_cancel: httpapi"
	}
	frame := protocol.NewCancel(id, participantFor(user), reason)
	frame.Payload.Cascade = body.Cascade
	a.submit(w, r, frame)
}

// ownedRequest resolves the verified user + path {id} and enforces ownership.
// On failure it has already written the response (404) and returns ok=false.
func (a *Adapter) ownedRequest(w http.ResponseWriter, r *http.Request) (VerifiedUser, string, bool) {
	user, _ := userFromCtx(r.Context())
	id := r.PathValue("id")
	if _, owned := a.findOwned(r.Context(), id, user.UserID); !owned {
		httpError(w, http.StatusNotFound, "session not found")
		return VerifiedUser{}, "", false
	}
	return user, id, true
}

// submit pushes a frame into the session and answers 202 (the outcome flows on
// the stream, not this response).
func (a *Adapter) submit(w http.ResponseWriter, r *http.Request, frame protocol.Frame) {
	if err := a.host.Submit(r.Context(), frame); err != nil {
		a.logger.Error("httpapi: submit", "session", frame.SessionID(), "kind", frame.Kind(), "err", err)
		httpError(w, http.StatusInternalServerError, "submit failed")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

// decodeBody decodes an optional JSON body (empty ⇒ zero value), capped.
func decodeBody(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes)).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
