package http

import (
	"bufio"
	"context"
	"encoding/json"
	stdhttp "net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/runtime"
)

// pausableReplay wraps a fakeStore and blocks ListEvents on a signal
// until the test releases it. Used by the overlap-dedupe test to
// freeze the replay window so a live frame can be injected into the
// already-registered subscriber.
type pausableReplay struct {
	inner   *fakeStore
	proceed chan struct{}
	once    sync.Once
}

func newPausableReplay(inner *fakeStore) *pausableReplay {
	return &pausableReplay{inner: inner, proceed: make(chan struct{})}
}

func (p *pausableReplay) ListEvents(ctx context.Context, sessionID string, opts runtime.ListEventsOpts) ([]runtime.EventRow, error) {
	<-p.proceed
	return p.inner.ListEvents(ctx, sessionID, opts)
}

func (p *pausableReplay) release() {
	p.once.Do(func() { close(p.proceed) })
}

// TestReconnect_ReplayLiveOverlap_NoDuplicate exercises the replay
// dedupe filter (sse.go: drop live frames with seq <= maxReplayedSeq).
// The handler registers the subscriber, then loadReplay blocks on
// pausableReplay; meanwhile the test appends + publishes seq 6 so
// the frame ends up in BOTH the replay output AND the live channel.
// Dedupe must drop the live copy — consumer sees seq 6 exactly once.
func TestReconnect_ReplayLiveOverlap_NoDuplicate(t *testing.T) {
	host := newFakeHost()
	replay := newPausableReplay(host.store)
	mux := stdhttp.NewServeMux()
	a, err := NewAdapter(Options{
		Mux:    mux,
		Auth:   allowAllAuth{},
		Codec:  protocol.NewCodec(),
		Replay: replay,
	})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = a.Run(runCtx, host) }()
	<-a.Mounted()
	a.MarkReady()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	openResp := doJSON(t, srv, "POST", "/api/v1/sessions", "tok", nil)
	var open OpenSessionResponse
	_ = json.NewDecoder(openResp.Body).Decode(&open)
	openResp.Body.Close()

	// Pre-seed seq 1..5 — these will appear in replay.
	seedEvents(host, open.SessionID, "agent-test", 5)

	// Open the stream in a goroutine — it blocks at loadReplay
	// because pausableReplay holds back ListEvents.
	type result struct {
		ids []string
		err error
	}
	done := make(chan result, 1)
	go func() {
		req, _ := stdhttp.NewRequest("GET",
			srv.URL+"/api/v1/sessions/"+open.SessionID+"/stream", nil)
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := srv.Client().Do(req)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer resp.Body.Close()
		r := bufio.NewReader(resp.Body)
		var ids []string
		// Read 6 events; if a duplicate appears we'll get 7 with
		// id=6 twice. Bound the read by a deadline.
		deadline := time.After(3 * time.Second)
		for len(ids) < 7 {
			select {
			case <-deadline:
				done <- result{ids: ids}
				return
			default:
			}
			ev, rerr := readSSEEvent(r)
			if rerr != nil {
				done <- result{ids: ids, err: rerr}
				return
			}
			if ev.event == "agent_message" && ev.id != "" {
				ids = append(ids, ev.id)
				if len(ids) >= 6 {
					// Read one more with a short deadline to catch
					// a duplicate that might still arrive.
					return7 := time.After(150 * time.Millisecond)
					_ = return7
					// Cheap sleep instead — readSSEEvent will block
					// forever if no more events come.
					go func() {
						time.Sleep(150 * time.Millisecond)
						resp.Body.Close()
					}()
				}
			}
		}
		done <- result{ids: ids}
	}()

	// Give the handler a moment to enter loadReplay.
	time.Sleep(50 * time.Millisecond)

	// Append + publish seq 6 — this is THE overlap. Append goes to
	// the store (so replay's ListEvents will return it); publish
	// goes to the bus (live channel).
	author := protocol.ParticipantInfo{ID: "agent-test", Kind: protocol.ParticipantAgent}
	live := protocol.NewAgentMessage(open.SessionID, author, "msg 6", 6, true)
	live.SetSeq(6)
	row, _, _ := runtime.FrameToEventRow(live, "agent-test")
	row.Seq = 6
	host.store.appendEvent(open.SessionID, row)
	host.publish(open.SessionID, live)

	// Release the replay query.
	replay.release()

	res := <-done
	// Exactly one occurrence of "6"; ids 1..6 in order.
	want := []string{"1", "2", "3", "4", "5", "6"}
	if len(res.ids) != len(want) {
		t.Fatalf("ids = %v, want %v (err=%v)", res.ids, want, res.err)
	}
	for i, id := range res.ids {
		if id != want[i] {
			t.Errorf("ids[%d] = %q, want %q", i, id, want[i])
		}
	}
	count6 := 0
	for _, id := range res.ids {
		if id == "6" {
			count6++
		}
	}
	if count6 != 1 {
		t.Errorf("seq 6 appeared %d times, want 1", count6)
	}
}

// TestReconnect_QueryParamFallback — the SPA passes the cursor as
// ?last_event_id=N because EventSource cannot set the header on
// initial open. Verify the server reads it as a fallback.
func TestReconnect_QueryParamFallback(t *testing.T) {
	host, srv := newTestServer(t, allowAllAuth{})
	openResp := doJSON(t, srv, "POST", "/api/v1/sessions", "tok", nil)
	var open OpenSessionResponse
	_ = json.NewDecoder(openResp.Body).Decode(&open)
	openResp.Body.Close()

	seedEvents(host, open.SessionID, "agent-test", 10)

	req, _ := stdhttp.NewRequest("GET",
		srv.URL+"/api/v1/sessions/"+open.SessionID+"/stream?last_event_id=7", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)

	// Replay should start from seq 8 (7+1) and continue through 10.
	for i := 8; i <= 10; i++ {
		ev, err := readSSEEvent(r)
		if err != nil {
			t.Fatalf("read seq %d: %v", i, err)
		}
		if ev.id != strconv.Itoa(i) {
			t.Errorf("event[%d] id = %q, want %d", i, ev.id, i)
		}
	}
}

// TestCORS_Allowlist exercises both an allowed and a denied origin.
// Denied origin gets Vary: Origin (per RFC) but no
// Access-Control-Allow-* headers; allowed origin gets the full set.
func TestCORS_Allowlist(t *testing.T) {
	host := newFakeHost()
	mux := stdhttp.NewServeMux()
	a, err := NewAdapter(Options{
		Mux:                mux,
		Auth:               allowAllAuth{},
		Codec:              protocol.NewCodec(),
		Replay:             host.store,
		CORSAllowedOrigins: []string{"http://127.0.0.1:10001"},
	})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = a.Run(runCtx, host) }()
	<-a.Mounted()
	a.MarkReady()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cases := []struct {
		name      string
		origin    string
		wantAllow bool
	}{
		{"allowed", "http://127.0.0.1:10001", true},
		{"denied — different port", "http://127.0.0.1:9999", false},
		{"denied — different host", "http://example.com", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := stdhttp.NewRequest("POST", srv.URL+"/api/v1/sessions", strings.NewReader("{}"))
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Authorization", "Bearer tok")
			req.Header.Set("Content-Type", "application/json")
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			resp.Body.Close()
			vary := resp.Header.Get("Vary")
			if !strings.Contains(vary, "Origin") {
				t.Errorf("Vary header missing Origin: %q", vary)
			}
			got := resp.Header.Get("Access-Control-Allow-Origin")
			if tc.wantAllow {
				if got != tc.origin {
					t.Errorf("Allow-Origin = %q, want %q", got, tc.origin)
				}
				if resp.Header.Get("Access-Control-Allow-Credentials") != "true" {
					t.Errorf("missing Allow-Credentials")
				}
			} else {
				if got != "" {
					t.Errorf("Allow-Origin set for denied origin: %q", got)
				}
				if resp.Header.Get("Access-Control-Allow-Credentials") != "" {
					t.Errorf("Allow-Credentials set for denied origin")
				}
			}
		})
	}
}

// TestHandlers_CloseIdempotent_OriginalTimestamp — close twice;
// assert the second response's closed_at equals the first.
func TestHandlers_CloseIdempotent_OriginalTimestamp(t *testing.T) {
	_, srv := newTestServer(t, allowAllAuth{})
	openResp := doJSON(t, srv, "POST", "/api/v1/sessions", "tok", nil)
	var open OpenSessionResponse
	_ = json.NewDecoder(openResp.Body).Decode(&open)
	openResp.Body.Close()

	resp1 := doJSON(t, srv, "POST", "/api/v1/sessions/"+open.SessionID+"/close", "tok",
		map[string]any{"reason": "user_end"})
	var body1 CloseSessionResponse
	_ = json.NewDecoder(resp1.Body).Decode(&body1)
	resp1.Body.Close()

	// Wait long enough that a second time.Now() would differ
	// noticeably if the handler regenerated it.
	time.Sleep(20 * time.Millisecond)

	resp2 := doJSON(t, srv, "POST", "/api/v1/sessions/"+open.SessionID+"/close", "tok",
		map[string]any{"reason": "again"})
	var body2 CloseSessionResponse
	_ = json.NewDecoder(resp2.Body).Decode(&body2)
	resp2.Body.Close()

	if !body1.ClosedAt.Equal(body2.ClosedAt) {
		t.Errorf("idempotent close timestamps differ: %v vs %v",
			body1.ClosedAt, body2.ClosedAt)
	}
}

// TestIsLoopback covers the IP-string variants the dual-stack
// listener can deliver. ::ffff:127.0.0.1 in particular requires
// net.ParseIP(...).IsLoopback() rather than a 127.x prefix check.
func TestIsLoopback(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:1234", true},
		{"127.255.0.1:1", true},
		{"[::1]:80", true},
		{"localhost:80", true},
		{"[::ffff:127.0.0.1]:1234", true},
		{"10.0.0.1:1234", false},
		{"example.com:80", false},
		{"[2001:db8::1]:443", false},
	}
	for _, tc := range cases {
		if got := isLoopback(tc.addr); got != tc.want {
			t.Errorf("isLoopback(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

// TestHandlers_PostToClosed_Returns409 — Submit on a closed session
// must surface as 409 session_closed, not 500 internal.
func TestHandlers_PostToClosed_Returns409(t *testing.T) {
	host, srv := newTestServer(t, allowAllAuth{})
	host.submitErr = runtime.ErrSessionClosed

	openResp := doJSON(t, srv, "POST", "/api/v1/sessions", "tok", nil)
	var open OpenSessionResponse
	_ = json.NewDecoder(openResp.Body).Decode(&open)
	openResp.Body.Close()

	resp := doJSON(t, srv, "POST", "/api/v1/sessions/"+open.SessionID+"/post", "tok",
		map[string]any{
			"kind":    "user_message",
			"author":  map[string]any{"id": "alice", "kind": "user"},
			"payload": map[string]any{"text": "hi"},
		})
	defer resp.Body.Close()
	if resp.StatusCode != stdhttp.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var env ErrorEnvelope
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if env.Error.Code != "session_closed" {
		t.Errorf("code = %q, want session_closed", env.Error.Code)
	}
}

// TestAPILifecycle_OpenSubscribePostListClose threads SC-002 as one
// connected story: an automation script using a generic HTTP client
// can complete the full lifecycle. The individual handler tests
// cover each shape; this test asserts they compose correctly.
func TestAPILifecycle_OpenSubscribePostListClose(t *testing.T) {
	host, srv := newTestServer(t, allowAllAuth{})

	// 1. Open
	resp := doJSON(t, srv, "POST", "/api/v1/sessions", "tok",
		map[string]any{"metadata": map[string]any{"label": "lifecycle"}})
	var open OpenSessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&open); err != nil {
		t.Fatalf("decode open: %v", err)
	}
	resp.Body.Close()
	if open.SessionID == "" || open.Status != runtime.StatusActive {
		t.Fatalf("open response = %+v", open)
	}

	// 2. Subscribe
	streamResp := openStream(t, srv, open.SessionID, "tok", "")
	defer streamResp.Body.Close()
	r := bufio.NewReader(streamResp.Body)

	// 3. Post a user message
	postResp := doJSON(t, srv, "POST", "/api/v1/sessions/"+open.SessionID+"/post", "tok",
		map[string]any{
			"kind":    "user_message",
			"author":  map[string]any{"id": "alice", "kind": "user"},
			"payload": map[string]any{"text": "hello"},
		})
	var post PostFrameResponse
	_ = json.NewDecoder(postResp.Body).Decode(&post)
	postResp.Body.Close()
	if post.FrameID == "" || post.SessionID != open.SessionID {
		t.Fatalf("post response = %+v", post)
	}

	// 4. Push a server-side reply through the bus and observe it
	// on the SSE stream. Drain whatever Submit-side echo arrived
	// first (the fakeHost.Submit fans the user_message back to
	// subscribers; a real runtime would too via the session pump).
	author := protocol.ParticipantInfo{ID: "agent-test", Kind: protocol.ParticipantAgent}
	live := protocol.NewAgentMessage(open.SessionID, author, "hi back", 0, true)
	live.SetSeq(1)
	host.publish(open.SessionID, live)
	gotAgent := false
	for !gotAgent {
		ev, err := readSSEEvent(r)
		if err != nil {
			t.Fatalf("read sse: %v", err)
		}
		if ev.event == "agent_message" {
			gotAgent = true
		}
	}

	// 5. List
	listResp := doJSON(t, srv, "GET", "/api/v1/sessions?status=active", "tok", nil)
	var list ListSessionsResponse
	_ = json.NewDecoder(listResp.Body).Decode(&list)
	listResp.Body.Close()
	found := false
	for _, s := range list.Sessions {
		if s.SessionID == open.SessionID {
			found = true
			if s.Metadata["label"] != "lifecycle" {
				t.Errorf("metadata roundtrip lost: %v", s.Metadata)
			}
		}
	}
	if !found {
		t.Errorf("listed sessions does not include %q", open.SessionID)
	}

	// 6. Close (idempotent)
	closeResp := doJSON(t, srv, "POST", "/api/v1/sessions/"+open.SessionID+"/close", "tok",
		map[string]any{"reason": "test_done"})
	var closed CloseSessionResponse
	_ = json.NewDecoder(closeResp.Body).Decode(&closed)
	closeResp.Body.Close()
	if closed.Status != runtime.StatusClosed || closed.ClosedAt.IsZero() {
		t.Errorf("close response = %+v", closed)
	}
}

// TestSlowConsumer_RecoversViaReconnect locks the second half of
// SC-007: a consumer that fell behind enough to trigger the bus's
// slow-consumer drop policy can recover the missed frames by
// reconnecting with Last-Event-ID. The persistence layer keeps a
// drop-resistant copy; the wire is just transport.
//
// Setup: persist N frames into the store (so they're replayable)
// AND publish them through the bus. Consumer A stalls and drops
// some frames in flight. A reconnects with Last-Event-ID=0, the
// replay path serves the entire persisted log.
func TestSlowConsumer_RecoversViaReconnect(t *testing.T) {
	host, srv := newTestServerOpts(t, Options{SlowConsumerGrace: 1})
	openResp := doJSON(t, srv, "POST", "/api/v1/sessions", "tok", nil)
	var open OpenSessionResponse
	_ = json.NewDecoder(openResp.Body).Decode(&open)
	openResp.Body.Close()

	// Connection A — never reads; its bus channel will fill, drops
	// will fire once we burst frames at it.
	respA := openStream(t, srv, open.SessionID, "tok", "")
	t.Cleanup(func() { respA.Body.Close() })

	// Burst 200 frames, both persisted and published. The
	// persisted log holds everything; the live channel for A
	// drops some.
	author := protocol.ParticipantInfo{ID: "agent-test", Kind: protocol.ParticipantAgent}
	const N = 200
	for i := 1; i <= N; i++ {
		f := protocol.NewAgentMessage(open.SessionID, author, "msg", i, false)
		f.SetSeq(i)
		row, _, _ := runtime.FrameToEventRow(f, "agent-test")
		row.Seq = i
		host.store.appendEvent(open.SessionID, row)
		host.publish(open.SessionID, f)
	}

	// Drop A's connection — its missed frames stay missed for that
	// connection forever. The session still has them in the store.
	respA.Body.Close()

	// Reconnect A with Last-Event-ID=0 → full replay.
	resp := openStream(t, srv, open.SessionID, "tok", "0")
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)

	seen := map[string]bool{}
	for len(seen) < N {
		ev, err := readSSEEvent(r)
		if err != nil {
			t.Fatalf("read after %d events: %v", len(seen), err)
		}
		if ev.event == "agent_message" && ev.id != "" {
			seen[ev.id] = true
		}
	}
	for i := 1; i <= N; i++ {
		if !seen[strconv.Itoa(i)] {
			t.Errorf("seq %d not recovered via reconnect", i)
		}
	}
}

// TestHandlers_RuntimeNotReady_Returns503 — until MarkReady fires,
// every /api/v1/* request must return 503 runtime_starting.
func TestHandlers_RuntimeNotReady_Returns503(t *testing.T) {
	host := newFakeHost()
	mux := stdhttp.NewServeMux()
	a, err := NewAdapter(Options{
		Mux:    mux,
		Auth:   allowAllAuth{},
		Codec:  protocol.NewCodec(),
		Replay: host.store,
	})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = a.Run(runCtx, host) }()
	<-a.Mounted()
	// NOT calling a.MarkReady().
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp := doJSON(t, srv, "POST", "/api/v1/sessions", "tok", nil)
	defer resp.Body.Close()
	if resp.StatusCode != stdhttp.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	var env ErrorEnvelope
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if env.Error.Code != "runtime_starting" {
		t.Errorf("code = %q, want runtime_starting", env.Error.Code)
	}
}
