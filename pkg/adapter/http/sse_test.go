package http

import (
	"bufio"
	"context"
	"encoding/json"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// newTestServerOpts wires up an Adapter with custom Options on a
// fresh httptest.Server. Used by SSE tests to tune heartbeat cadence.
func newTestServerOpts(t *testing.T, opts Options) (*fakeHost, *httptest.Server) {
	t.Helper()
	host := newFakeHost()
	if opts.Mux == nil {
		opts.Mux = stdhttp.NewServeMux()
	}
	if opts.Auth == nil {
		opts.Auth = allowAllAuth{}
	}
	if opts.Codec == nil {
		opts.Codec = protocol.NewCodec()
	}
	if opts.Replay == nil {
		opts.Replay = host.store
	}
	a, err := NewAdapter(opts)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = a.Run(runCtx, host) }()
	<-a.Mounted()
	a.MarkReady()
	srv := httptest.NewServer(opts.Mux)
	t.Cleanup(srv.Close)
	return host, srv
}

// openStream issues an authenticated GET on the SSE endpoint with
// the given Last-Event-ID and returns the response. Caller closes
// the body to drop the subscription.
func openStream(t *testing.T, srv *httptest.Server, sessionID, token, lastEventID string) *stdhttp.Response {
	t.Helper()
	req, err := stdhttp.NewRequest("GET", srv.URL+"/api/v1/sessions/"+sessionID+"/stream", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

// sseEvent is a parsed SSE event read off the wire.
type sseEvent struct {
	id      string
	event   string
	data    string
	comment string
}

// readSSEEvent reads one event from a bufio.Reader, returning the
// parsed event or an error. Comment lines (`:`) are returned as
// events with `comment` set and the other fields empty.
func readSSEEvent(r *bufio.Reader) (sseEvent, error) {
	var ev sseEvent
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return ev, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			return ev, nil
		}
		switch {
		case strings.HasPrefix(line, "id:"):
			ev.id = strings.TrimSpace(line[len("id:"):])
		case strings.HasPrefix(line, "event:"):
			ev.event = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			ev.data = strings.TrimSpace(line[len("data:"):])
		case strings.HasPrefix(line, ":"):
			ev.comment = strings.TrimSpace(line[1:])
		}
	}
}

func TestSSE_HeaderShape(t *testing.T) {
	host, srv := newTestServer(t, allowAllAuth{})
	openResp := doJSON(t, srv, "POST", "/api/v1/sessions", "tok", nil)
	var open OpenSessionResponse
	_ = json.NewDecoder(openResp.Body).Decode(&open)
	openResp.Body.Close()
	_ = host

	resp := openStream(t, srv, open.SessionID, "tok", "")
	defer resp.Body.Close()

	if resp.StatusCode != stdhttp.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("Content-Type = %q", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q", got)
	}
}

func TestSSE_FrameSerialisation(t *testing.T) {
	host, srv := newTestServer(t, allowAllAuth{})
	openResp := doJSON(t, srv, "POST", "/api/v1/sessions", "tok", nil)
	var open OpenSessionResponse
	_ = json.NewDecoder(openResp.Body).Decode(&open)
	openResp.Body.Close()

	resp := openStream(t, srv, open.SessionID, "tok", "")
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)

	author := protocol.ParticipantInfo{ID: "agent-test", Kind: protocol.ParticipantAgent}
	host.publish(open.SessionID, protocol.NewReasoning(open.SessionID, author, "thinking", 0, false))
	host.publish(open.SessionID, protocol.NewAgentMessage(open.SessionID, author, "hello", 0, false))
	host.publish(open.SessionID, protocol.NewAgentMessage(open.SessionID, author, " world", 1, true))

	want := []string{"reasoning", "agent_message", "agent_message"}
	for i, w := range want {
		ev, err := readSSEEvent(r)
		if err != nil {
			t.Fatalf("readSSEEvent[%d]: %v", i, err)
		}
		if ev.event != w {
			t.Errorf("event[%d] = %q, want %q", i, ev.event, w)
		}
		if strings.Contains(ev.data, "\n") {
			t.Errorf("event[%d] data contains newline: %q", i, ev.data)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(ev.data), &got); err != nil {
			t.Fatalf("event[%d] data not JSON: %v (%q)", i, err, ev.data)
		}
		if got["kind"] != w {
			t.Errorf("event[%d] kind = %v, want %q", i, got["kind"], w)
		}
	}
}

func TestSSE_HeartbeatComment(t *testing.T) {
	host, srv := newTestServerOpts(t, Options{HeartbeatInterval: 1}) // 1 second
	openResp := doJSON(t, srv, "POST", "/api/v1/sessions", "tok", nil)
	var open OpenSessionResponse
	_ = json.NewDecoder(openResp.Body).Decode(&open)
	openResp.Body.Close()
	_ = host

	resp := openStream(t, srv, open.SessionID, "tok", "")
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)

	// Set a deadline so a wedged read fails the test rather than hanging.
	doneCh := make(chan struct{})
	go func() {
		<-time.After(5 * time.Second)
		select {
		case <-doneCh:
		default:
			resp.Body.Close()
		}
	}()
	defer close(doneCh)

	ev, err := readSSEEvent(r)
	if err != nil {
		t.Fatalf("readSSEEvent: %v", err)
	}
	if ev.comment != "heartbeat" {
		t.Errorf("expected heartbeat comment, got %#v", ev)
	}

	// Heartbeats must not be persisted as session_events.
	if got := host.store.events[open.SessionID]; len(got) != 0 {
		t.Errorf("heartbeat persisted to event store: %v", got)
	}
}

// TestSSE_SlowConsumer_DropsFramesNotBlocks — two SSE consumers on
// the same session; consumer A stops reading from its socket so its
// per-connection channel fills up; consumer B keeps draining; assert
// B receives every frame, A's bus drops at the 50ms grace.
//
// This exercises sessionBus.deliver: per-connection drop policy
// means a stalled consumer cannot stall its peers (FR-026 / SC-007).
func TestSSE_SlowConsumer_DropsFramesNotBlocks(t *testing.T) {
	// Tighter grace so the test doesn't take forever; the contract
	// pins 50ms in production but the option is the seam for tests.
	host, srv := newTestServerOpts(t, Options{SlowConsumerGrace: 5})
	openResp := doJSON(t, srv, "POST", "/api/v1/sessions", "tok", nil)
	var open OpenSessionResponse
	_ = json.NewDecoder(openResp.Body).Decode(&open)
	openResp.Body.Close()

	// Consumer A — never reads; we just hold the connection open
	// and let its bus channel back up. The handshake completes
	// (status 200 + headers) after attachSubscriber registers, so
	// once openStream returns the subscriber is on the bus.
	respA := openStream(t, srv, open.SessionID, "tok", "")
	defer respA.Body.Close()

	// Consumer B — reads frames as they arrive.
	respB := openStream(t, srv, open.SessionID, "tok", "")
	defer respB.Body.Close()
	rB := bufio.NewReader(respB.Body)

	author := protocol.ParticipantInfo{ID: "agent-test", Kind: protocol.ParticipantAgent}
	const N = 200
	for i := 0; i < N; i++ {
		f := protocol.NewAgentMessage(open.SessionID, author, "msg", i, false)
		f.SetSeq(i + 1) // unique id per frame
		host.publish(open.SessionID, f)
	}

	// Consumer B should receive every frame; if A blocked the
	// bus, B's read would deadlock and the test would time out.
	got := 0
	for got < N {
		ev, err := readSSEEvent(rB)
		if err != nil {
			t.Fatalf("consumer B read after %d events: %v", got, err)
		}
		if ev.event == "agent_message" {
			got++
		}
	}
	if got != N {
		t.Errorf("consumer B got %d events, want %d", got, N)
	}
}

func TestSSE_HeartbeatResetsOnFrame(t *testing.T) {
	host, srv := newTestServerOpts(t, Options{HeartbeatInterval: 1})
	openResp := doJSON(t, srv, "POST", "/api/v1/sessions", "tok", nil)
	var open OpenSessionResponse
	_ = json.NewDecoder(openResp.Body).Decode(&open)
	openResp.Body.Close()

	resp := openStream(t, srv, open.SessionID, "tok", "")
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)

	// Push a frame quickly so the heartbeat ticker resets. Then the
	// next event we read should be that frame, not a heartbeat that
	// landed first.
	author := protocol.ParticipantInfo{ID: "agent-test", Kind: protocol.ParticipantAgent}
	host.publish(open.SessionID, protocol.NewAgentMessage(open.SessionID, author, "early", 0, true))

	ev, err := readSSEEvent(r)
	if err != nil {
		t.Fatalf("readSSEEvent: %v", err)
	}
	if ev.event != "agent_message" {
		t.Errorf("first event = %q, want agent_message (no heartbeat first); ev=%#v", ev.event, ev)
	}
}
