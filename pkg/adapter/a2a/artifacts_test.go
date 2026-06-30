package a2a

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	artifactext "github.com/hugr-lab/hugen/pkg/extension/artifact"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// TestArtifactConstsMatchExtension guards the local literals against the
// artifact extension they mirror (kept local to avoid an adapter→extension
// import in non-test code).
func TestArtifactConstsMatchExtension(t *testing.T) {
	if artifactOpProduced != artifactext.OpProduced {
		t.Errorf("artifactOpProduced = %q, want %q (extension drift)", artifactOpProduced, artifactext.OpProduced)
	}
	if artifactExtensionName != "artifact" {
		t.Errorf("artifactExtensionName = %q, want artifact", artifactExtensionName)
	}
}

func TestArtifactDownload_SignedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fpath := filepath.Join(dir, "report.html")
	if err := os.WriteFile(fpath, []byte("<h1>hi</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	resolve := func(root, id string) (string, error) {
		if root == "ses-1" && id == "art-1" {
			return fpath, nil
		}
		return "", os.ErrNotExist
	}
	const secret = "sek"
	h := artifactDownloadHandler(secret, resolve, quietLogger())

	serve := func(u string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, u, nil))
		return rec
	}

	// Correct signed URL → 200 + body.
	ok := signedArtifactURL("http://x", secret, "ses-1", "art-1", time.Now())
	if rec := serve(ok); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "hi") {
		t.Fatalf("valid link: code=%d body=%q", rec.Code, rec.Body.String())
	}
	// Tampered signature → 401.
	if rec := serve(ok + "deadbeef"); rec.Code != http.StatusUnauthorized {
		t.Errorf("tampered sig → %d, want 401", rec.Code)
	}
	// Expired link (signed with a past expiry) → 401.
	past := signedArtifactURL("http://x", secret, "ses-1", "art-1", time.Now().Add(-2*artifactURLTTL))
	if rec := serve(past); rec.Code != http.StatusUnauthorized {
		t.Errorf("expired link → %d, want 401", rec.Code)
	}
	// Valid signature, missing artifact → 404.
	gone := signedArtifactURL("http://x", secret, "ses-1", "art-gone", time.Now())
	if rec := serve(gone); rec.Code != http.StatusNotFound {
		t.Errorf("missing artifact → %d, want 404", rec.Code)
	}
	// Wrong secret can't forge a link → 401.
	forged := signedArtifactURL("http://x", "other", "ses-1", "art-1", time.Now())
	if rec := serve(forged); rec.Code != http.StatusUnauthorized {
		t.Errorf("forged (wrong secret) → %d, want 401", rec.Code)
	}
	// Malformed path (no id) → 400.
	if rec := serve("http://x" + artifactPathPrefix + "onlyroot"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad path → %d, want 400", rec.Code)
	}
}

// artifactFrame builds the published-artifact ExtensionFrame the executor maps.
func artifactFrame(root string, ref protocol.ArtifactRef) protocol.Frame {
	data, _ := json.Marshal(ref)
	return protocol.NewExtensionFrame(root, serviceParticipant(),
		artifactExtensionName, protocol.CategoryMarker, artifactOpProduced, data)
}

// TestSessionExecutor_EmitsArtifact drives a turn that publishes an artifact:
// the executor materialises a Task and emits a TaskArtifactUpdateEvent whose
// FilePart points at the by-ref signed URL with the artifact's filename + mime.
func TestSessionExecutor_EmitsArtifact(t *testing.T) {
	io := &fakeFrameIO{ch: make(chan protocol.Frame, 8)}
	reg := newContextRegistry(&fakeRootStore{}, quietLogger())
	urlBuilder := func(root, id string) string {
		return "https://host/a2a/artifacts/" + root + "/" + id + "?exp=1&sig=abc"
	}
	e := newSessionExecutor(quietLogger(), reg, io, serviceParticipant(), urlBuilder)

	io.ch <- artifactFrame("root-1", protocol.ArtifactRef{ID: "art-1", Name: "report.html", MIME: "text/html"})
	io.ch <- finalFrame("root-1", "Here is your report.", nil, nil)

	execCtx := &a2asrv.ExecutorContext{
		Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("build a report")),
		ContextID: "ctx-1",
		TaskID:    a2a.NewTaskID(),
	}
	events, err := collectErr(e.Execute(context.Background(), execCtx))
	if err != nil {
		t.Fatalf("artifact turn: %v", err)
	}
	// submitted (materialised by the artifact) + artifact event + completed.
	var art *a2a.TaskArtifactUpdateEvent
	tasks := 0
	for _, ev := range events {
		switch v := ev.(type) {
		case *a2a.Task:
			tasks++
		case *a2a.TaskArtifactUpdateEvent:
			art = v
		}
	}
	if tasks != 1 {
		t.Errorf("materialised %d Tasks, want 1", tasks)
	}
	if art == nil {
		t.Fatalf("no TaskArtifactUpdateEvent in %d events", len(events))
	}
	if art.Artifact == nil || len(art.Artifact.Parts) != 1 {
		t.Fatalf("artifact parts = %+v, want one", art.Artifact)
	}
	p := art.Artifact.Parts[0]
	if p.Filename != "report.html" {
		t.Errorf("filename = %q, want report.html", p.Filename)
	}
	if p.MediaType != "text/html" {
		t.Errorf("media type = %q, want text/html", p.MediaType)
	}
	if u := string(p.URL()); !strings.Contains(u, "art-1") || !strings.Contains(u, "/a2a/artifacts/") {
		t.Errorf("FilePart URL = %q, want the signed by-ref download URL", u)
	}
}

// TestSessionExecutor_NoArtifactResolver_SkipsArtifact guards that without a
// resolver (artifacts disabled) a published-artifact frame is ignored.
func TestSessionExecutor_NoArtifactResolver_SkipsArtifact(t *testing.T) {
	io := &fakeFrameIO{ch: make(chan protocol.Frame, 8)}
	reg := newContextRegistry(&fakeRootStore{}, quietLogger())
	e := newSessionExecutor(quietLogger(), reg, io, serviceParticipant(), nil) // no URL builder

	io.ch <- artifactFrame("root-1", protocol.ArtifactRef{ID: "art-1", Name: "report.html"})
	io.ch <- finalFrame("root-1", "done", nil, nil)

	execCtx := &a2asrv.ExecutorContext{
		Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("x")),
		ContextID: "ctx-1",
		TaskID:    a2a.NewTaskID(),
	}
	events, err := collectErr(e.Execute(context.Background(), execCtx))
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	// No artifact resolver → no Task materialised, finishes as a bare Message.
	if len(events) != 1 {
		t.Fatalf("yielded %d events, want 1 (bare message, artifact ignored)", len(events))
	}
	if _, ok := events[0].(*a2a.Message); !ok {
		t.Errorf("event0 = %T, want *a2a.Message", events[0])
	}
}
