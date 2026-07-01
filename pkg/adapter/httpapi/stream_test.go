package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
)

func TestAlreadyReplayed(t *testing.T) {
	cases := []struct {
		seq, max int
		want     bool
	}{
		{0, 5, false},  // streaming chunk (seq 0) always passes
		{3, 5, true},   // persisted, in the replay window
		{5, 5, true},   // boundary
		{6, 5, false},  // new persisted frame
		{0, 0, false},  // chunk, nothing replayed
	}
	for _, c := range cases {
		if got := alreadyReplayed(c.seq, c.max); got != c.want {
			t.Errorf("alreadyReplayed(%d,%d) = %v, want %v", c.seq, c.max, got, c.want)
		}
	}
}

func TestParseLastEventID(t *testing.T) {
	mk := func(header, query string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/s?from="+query, nil)
		if header != "" {
			r.Header.Set("Last-Event-ID", header)
		}
		return r
	}
	if got := parseLastEventID(mk("42", "")); got != 42 {
		t.Errorf("header: got %d, want 42", got)
	}
	if got := parseLastEventID(mk("", "7")); got != 7 {
		t.Errorf("query: got %d, want 7", got)
	}
	if got := parseLastEventID(mk("99", "7")); got != 99 {
		t.Errorf("header wins: got %d, want 99", got)
	}
	if got := parseLastEventID(mk("nope", "")); got != 0 {
		t.Errorf("invalid: got %d, want 0", got)
	}
}

func TestWriteSSEFrame_Format(t *testing.T) {
	f := protocol.NewUserMessage("ses-1", protocol.ParticipantInfo{ID: "u1", Kind: protocol.ParticipantUser}, "hello")
	var sb strings.Builder
	rc := http.NewResponseController(httptest.NewRecorder())
	if err := writeSSEFrame(&sb, rc, 7, f); err != nil {
		t.Fatalf("writeSSEFrame: %v", err)
	}
	out := sb.String()
	if !strings.HasPrefix(out, "id: 7\n") {
		t.Errorf("missing id line: %q", out)
	}
	if !strings.Contains(out, "\ndata: ") || !strings.HasSuffix(out, "\n\n") {
		t.Errorf("malformed SSE framing: %q", out)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("payload not in data: %q", out)
	}
	// seq 0 (streaming chunk) → no id line.
	sb.Reset()
	if err := writeSSEFrame(&sb, rc, 0, f); err != nil {
		t.Fatalf("writeSSEFrame(seq0): %v", err)
	}
	if strings.Contains(sb.String(), "id:") {
		t.Errorf("seq 0 wrote an id line: %q", sb.String())
	}
}

// TestStream_LiveFrame drives the handler: a live frame pushed on the fanout
// channel reaches the SSE body; client-disconnect (ctx cancel) ends the stream.
func TestStream_LiveFrame(t *testing.T) {
	sub := make(chan protocol.Frame, 4)
	fake := &fakeHost{
		sessions: []session.SessionSummary{ownedSummary("ses-mine", "active", "local")},
		sub:      sub,
	}
	a := New(WithLogger(quietLogger()))
	a.host = fake
	a.lifecycleCtx = context.Background()
	mux := http.NewServeMux()
	if err := a.mount(mux, false); err != nil {
		t.Fatalf("mount: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/ses-mine/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		mux.ServeHTTP(rec, req)
		close(done)
	}()

	sub <- protocol.NewUserMessage("ses-mine", protocol.ParticipantInfo{ID: "u1", Kind: protocol.ParticipantUser}, "live-frame-xyz")
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream handler did not return after ctx cancel")
	}

	body := rec.Body.String()
	if !strings.Contains(body, "live-frame-xyz") {
		t.Errorf("live frame not in stream body: %q", body)
	}
	if rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", rec.Header().Get("Content-Type"))
	}
}
