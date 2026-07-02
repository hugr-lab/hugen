package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
)

func postJSON(mux *http.ServeMux, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestSendMessage_SubmitsUserMessage(t *testing.T) {
	_, fake, mux := sessionsAdapter(t, []session.SessionSummary{ownedSummary("ses-mine", "active", "local")})
	rec := postJSON(mux, "/v1/sessions/ses-mine/messages", `{"text":"hello"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202", rec.Code)
	}
	if len(fake.submitted) != 1 {
		t.Fatalf("submitted %d frames, want 1", len(fake.submitted))
	}
	um, ok := fake.submitted[0].(*protocol.UserMessage)
	if !ok {
		t.Fatalf("submitted %T, want *protocol.UserMessage", fake.submitted[0])
	}
	if um.SessionID() != "ses-mine" || um.Payload.Text != "hello" {
		t.Errorf("message = session %q text %q, want ses-mine/hello", um.SessionID(), um.Payload.Text)
	}
}

func TestSendMessage_EmptyText(t *testing.T) {
	_, fake, mux := sessionsAdapter(t, []session.SessionSummary{ownedSummary("ses-mine", "active", "local")})
	rec := postJSON(mux, "/v1/sessions/ses-mine/messages", `{"text":"   "}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty text: status %d, want 400", rec.Code)
	}
	if len(fake.submitted) != 0 {
		t.Errorf("empty text submitted a frame: %v", fake.submitted)
	}
}

func TestSendMessage_NotOwned(t *testing.T) {
	_, fake, mux := sessionsAdapter(t, []session.SessionSummary{ownedSummary("ses-other", "active", "someone-else")})
	rec := postJSON(mux, "/v1/sessions/ses-other/messages", `{"text":"hi"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("not-owned: status %d, want 404", rec.Code)
	}
	if len(fake.submitted) != 0 {
		t.Errorf("not-owned submitted a frame: %v", fake.submitted)
	}
}

func TestInquiryResponse_SubmitsFrame(t *testing.T) {
	_, fake, mux := sessionsAdapter(t, []session.SessionSummary{ownedSummary("ses-mine", "wait_user_input", "local")})
	rec := postJSON(mux, "/v1/sessions/ses-mine/inquiry", `{"request_id":"req-1","response":"EMEA"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202", rec.Code)
	}
	ir, ok := fake.submitted[0].(*protocol.InquiryResponse)
	if !ok {
		t.Fatalf("submitted %T, want *protocol.InquiryResponse", fake.submitted[0])
	}
	if ir.Payload.RequestID != "req-1" || ir.Payload.Response != "EMEA" {
		t.Errorf("inquiry = %+v, want req-1/EMEA", ir.Payload)
	}
}

func TestInquiryResponse_MissingRequestID(t *testing.T) {
	_, fake, mux := sessionsAdapter(t, []session.SessionSummary{ownedSummary("ses-mine", "active", "local")})
	rec := postJSON(mux, "/v1/sessions/ses-mine/inquiry", `{"response":"x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing request_id: status %d, want 400", rec.Code)
	}
	if len(fake.submitted) != 0 {
		t.Errorf("submitted without request_id: %v", fake.submitted)
	}
}

func TestCancel_SubmitsCancel(t *testing.T) {
	_, fake, mux := sessionsAdapter(t, []session.SessionSummary{ownedSummary("ses-mine", "active", "local")})
	rec := postJSON(mux, "/v1/sessions/ses-mine/cancel", `{"cascade":true}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202", rec.Code)
	}
	c, ok := fake.submitted[0].(*protocol.Cancel)
	if !ok {
		t.Fatalf("submitted %T, want *protocol.Cancel", fake.submitted[0])
	}
	if !c.Payload.Cascade {
		t.Error("cancel cascade = false, want true")
	}
}

func TestCancel_EmptyBodyOK(t *testing.T) {
	// cancel body is optional — an empty POST still submits a Cancel.
	_, fake, mux := sessionsAdapter(t, []session.SessionSummary{ownedSummary("ses-mine", "active", "local")})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/ses-mine/cancel", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("empty cancel: status %d, want 202", rec.Code)
	}
	if _, ok := fake.submitted[0].(*protocol.Cancel); !ok {
		t.Fatalf("submitted %T, want *protocol.Cancel", fake.submitted[0])
	}
}
