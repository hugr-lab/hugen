package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/session/manager"
)

// fakeHost serves canned sessions and records closes. Only the read/close
// methods the H3 handlers reach are implemented; the rest stay nil-embedded.
type fakeHost struct {
	manager.AdapterHost
	sessions  []session.SessionSummary
	closed    []string
	submitted []protocol.Frame
	submitErr error
}

func (f *fakeHost) Submit(_ context.Context, frame protocol.Frame) error {
	if f.submitErr != nil {
		return f.submitErr
	}
	f.submitted = append(f.submitted, frame)
	return nil
}

func (f *fakeHost) ListSessions(_ context.Context, status string) ([]session.SessionSummary, error) {
	if status == "" {
		return f.sessions, nil
	}
	var out []session.SessionSummary
	for _, s := range f.sessions {
		if s.Status == status {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeHost) CloseSession(_ context.Context, id, _ string) (time.Time, error) {
	f.closed = append(f.closed, id)
	return time.Time{}, nil
}

func (f *fakeHost) Logger() *slog.Logger { return quietLogger() }

// sessionsAdapter wires an allow-open (devUser="local") adapter over a fakeHost.
func sessionsAdapter(t *testing.T, sessions []session.SessionSummary) (*Adapter, *fakeHost, *http.ServeMux) {
	t.Helper()
	fake := &fakeHost{sessions: sessions}
	a := New(WithLogger(quietLogger()))
	a.host = fake
	a.lifecycleCtx = context.Background()
	mux := http.NewServeMux()
	if err := a.mount(mux, false); err != nil {
		t.Fatalf("mount: %v", err)
	}
	return a, fake, mux
}

func ownedSummary(id, status, owner string) session.SessionSummary {
	return session.SessionSummary{ID: id, Status: status, Metadata: map[string]any{ownerMetaKey: owner}}
}

func TestListSessions_OwnerScoped(t *testing.T) {
	_, _, mux := sessionsAdapter(t, []session.SessionSummary{
		ownedSummary("ses-mine1", "active", "local"),
		ownedSummary("ses-other", "active", "someone-else"),
		ownedSummary("ses-mine2", "idle", "local"),
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	var got []sessionDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2 (only mine)", len(got))
	}
	for _, s := range got {
		if s.ID == "ses-other" {
			t.Errorf("leaked another owner's session: %s", s.ID)
		}
	}
}

func TestGetSession_OwnershipEnforced(t *testing.T) {
	_, _, mux := sessionsAdapter(t, []session.SessionSummary{
		ownedSummary("ses-mine", "active", "local"),
		ownedSummary("ses-other", "active", "someone-else"),
	})
	// Owned → 200.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions/ses-mine", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("owned GET: status %d, want 200", rec.Code)
	}
	// Not owned → 404 (don't leak existence).
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions/ses-other", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("not-owned GET: status %d, want 404", rec.Code)
	}
}

func TestDeleteSession_OwnershipEnforced(t *testing.T) {
	_, fake, mux := sessionsAdapter(t, []session.SessionSummary{
		ownedSummary("ses-mine", "active", "local"),
		ownedSummary("ses-other", "active", "someone-else"),
	})
	// Owned → 204 + closed.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/sessions/ses-mine", nil))
	if rec.Code != http.StatusNoContent {
		t.Errorf("owned DELETE: status %d, want 204", rec.Code)
	}
	if len(fake.closed) != 1 || fake.closed[0] != "ses-mine" {
		t.Errorf("closed = %v, want [ses-mine]", fake.closed)
	}
	// Not owned → 404 + NOT closed.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/sessions/ses-other", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("not-owned DELETE: status %d, want 404", rec.Code)
	}
	if len(fake.closed) != 1 {
		t.Errorf("not-owned DELETE closed a session: %v", fake.closed)
	}
}

func TestOwnedBy(t *testing.T) {
	s := ownedSummary("s1", "active", "u1")
	if !ownedBy(s, "u1") {
		t.Error("ownedBy(u1) = false, want true")
	}
	if ownedBy(s, "u2") {
		t.Error("ownedBy(u2) = true, want false")
	}
	if ownedBy(s, "") {
		t.Error("ownedBy(empty) = true, want false")
	}
	if ownedBy(session.SessionSummary{ID: "s2"}, "u1") {
		t.Error("ownedBy on no-metadata session = true, want false")
	}
}
