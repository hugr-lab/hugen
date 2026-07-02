package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

type fakeArtifacts struct {
	refs      []protocol.ArtifactRef
	pathFor   map[string]string
	ingested  []string
	ingestRef protocol.ArtifactRef
}

func (f *fakeArtifacts) List(string) ([]protocol.ArtifactRef, error) { return f.refs, nil }

func (f *fakeArtifacts) Path(_, id string) (string, error) {
	if p, ok := f.pathFor[id]; ok {
		return p, nil
	}
	return "", errors.New("not found")
}

func (f *fakeArtifacts) Ingest(_, _, name string) (protocol.ArtifactRef, error) {
	f.ingested = append(f.ingested, name)
	return f.ingestRef, nil
}

// resourcesAdapter wires an allow-open adapter over a fakeHost + optional
// artifact store, owning session "ses-mine".
func resourcesAdapter(t *testing.T, host *fakeHost, arts ArtifactStore) *http.ServeMux {
	t.Helper()
	a := New(WithLogger(quietLogger()))
	a.host = host
	a.lifecycleCtx = context.Background()
	a.artifacts = arts
	mux := http.NewServeMux()
	if err := a.mount(mux, false); err != nil {
		t.Fatalf("mount: %v", err)
	}
	return mux
}

func TestListEvents(t *testing.T) {
	host := &fakeHost{
		sessions: []session.SessionSummary{ownedSummary("ses-mine", "active", "local")},
		events: []store.EventRow{
			{Seq: 1, EventType: "user_message", Content: "hi"},
			{Seq: 2, EventType: "agent_message", Content: "yo"},
		},
	}
	mux := resourcesAdapter(t, host, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions/ses-mine/events", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	var rows []store.EventRow
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 2 || rows[0].Seq != 1 {
		t.Errorf("events = %+v, want 2 rows starting seq 1", rows)
	}
}

func TestArtifacts_DisabledWhenNoStore(t *testing.T) {
	host := &fakeHost{sessions: []session.SessionSummary{ownedSummary("ses-mine", "active", "local")}}
	mux := resourcesAdapter(t, host, nil) // no artifact store
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions/ses-mine/artifacts", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("no store: status %d, want 501", rec.Code)
	}
}

func TestListArtifacts(t *testing.T) {
	host := &fakeHost{sessions: []session.SessionSummary{ownedSummary("ses-mine", "active", "local")}}
	arts := &fakeArtifacts{refs: []protocol.ArtifactRef{{ID: "a1", Name: "report.html", Size: 42}}}
	mux := resourcesAdapter(t, host, arts)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions/ses-mine/artifacts", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	var refs []protocol.ArtifactRef
	if err := json.Unmarshal(rec.Body.Bytes(), &refs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(refs) != 1 || refs[0].ID != "a1" {
		t.Errorf("refs = %+v, want [a1]", refs)
	}
}

func TestGetArtifact(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "art-*")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("ARTIFACT-BODY")
	_ = f.Close()

	host := &fakeHost{sessions: []session.SessionSummary{ownedSummary("ses-mine", "active", "local")}}
	arts := &fakeArtifacts{pathFor: map[string]string{"a1": f.Name()}}
	mux := resourcesAdapter(t, host, arts)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions/ses-mine/artifacts/a1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if rec.Body.String() != "ARTIFACT-BODY" {
		t.Errorf("body = %q, want ARTIFACT-BODY", rec.Body.String())
	}
	// unknown artifact → 404
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions/ses-mine/artifacts/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown artifact: status %d, want 404", rec.Code)
	}
}

func TestIngestArtifact(t *testing.T) {
	host := &fakeHost{sessions: []session.SessionSummary{ownedSummary("ses-mine", "active", "local")}}
	arts := &fakeArtifacts{ingestRef: protocol.ArtifactRef{ID: "new1", Name: "upload.txt"}}
	mux := resourcesAdapter(t, host, arts)

	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/ses-mine/artifacts?name=upload.txt", strings.NewReader("file-content"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, want 201", rec.Code)
	}
	if len(arts.ingested) != 1 || arts.ingested[0] != "upload.txt" {
		t.Errorf("ingested = %v, want [upload.txt]", arts.ingested)
	}
	var ref protocol.ArtifactRef
	if err := json.Unmarshal(rec.Body.Bytes(), &ref); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ref.ID != "new1" {
		t.Errorf("ref = %+v, want id new1", ref)
	}
}
