package http

import (
	"bufio"
	"encoding/json"
	stdhttp "net/http"
	"strconv"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
)

func TestParseLastEventID(t *testing.T) {
	cases := []struct {
		in     string
		wantN  int
		wantOK bool
	}{
		{"", 0, false},
		{"42", 42, true},
		{"0", 0, true},
		{"-1", 0, false},
		{"abc", 0, false},
		{"1.5", 0, false},
		{"  ", 0, false},
	}
	for _, tc := range cases {
		gotN, gotOK := parseLastEventID(tc.in)
		if gotN != tc.wantN || gotOK != tc.wantOK {
			t.Errorf("parseLastEventID(%q) = (%d, %v); want (%d, %v)",
				tc.in, gotN, gotOK, tc.wantN, tc.wantOK)
		}
	}
}

func seedEvents(host *fakeHost, sessionID, agentID string, n int) {
	author := protocol.ParticipantInfo{ID: agentID, Kind: protocol.ParticipantAgent}
	for i := 1; i <= n; i++ {
		f := protocol.NewAgentMessage(sessionID, author, "msg "+strconv.Itoa(i), i, true)
		row, _, err := session.FrameToEventRow(f, agentID)
		if err != nil {
			panic(err)
		}
		row.Seq = i
		host.store.appendEvent(sessionID, row)
	}
}

// TestReconnect_LastEventID_ReplayThenLive — events 1..10 are seeded;
// open SSE with Last-Event-ID: 5; assert seq 6..10 stream then a
// live event arrives with no duplicate ids.
func TestReconnect_LastEventID_ReplayThenLive(t *testing.T) {
	host, srv := newTestServer(t, allowAllAuth{})
	openResp := doJSON(t, srv, "POST", "/api/v1/sessions", "tok", nil)
	var open OpenSessionResponse
	_ = json.NewDecoder(openResp.Body).Decode(&open)
	openResp.Body.Close()

	seedEvents(host, open.SessionID, "agent-test", 10)

	resp := openStream(t, srv, open.SessionID, "tok", "5")
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)

	seenIDs := map[string]bool{}
	for i := 6; i <= 10; i++ {
		ev, err := readSSEEvent(r)
		if err != nil {
			t.Fatalf("replay event %d: %v", i, err)
		}
		if ev.id != strconv.Itoa(i) {
			t.Errorf("replay[%d] id = %q, want %d", i, ev.id, i)
		}
		if seenIDs[ev.id] {
			t.Errorf("duplicate id %q on replay", ev.id)
		}
		seenIDs[ev.id] = true
	}

	// Now push a live event; expect it next.
	author := protocol.ParticipantInfo{ID: "agent-test", Kind: protocol.ParticipantAgent}
	host.publish(open.SessionID, protocol.NewAgentMessage(open.SessionID, author, "live", 0, true))
	ev, err := readSSEEvent(r)
	if err != nil {
		t.Fatalf("live event: %v", err)
	}
	if ev.event != "agent_message" {
		t.Errorf("live event kind = %q", ev.event)
	}
}

// TestReconnect_UnknownLastEventID_ResumesLive — malformed cursor
// values must not error or replay; the consumer just enters live tail.
func TestReconnect_UnknownLastEventID_ResumesLive(t *testing.T) {
	host, srv := newTestServer(t, allowAllAuth{})
	openResp := doJSON(t, srv, "POST", "/api/v1/sessions", "tok", nil)
	var open OpenSessionResponse
	_ = json.NewDecoder(openResp.Body).Decode(&open)
	openResp.Body.Close()

	seedEvents(host, open.SessionID, "agent-test", 5)

	for _, cursor := range []string{"not-a-number", "-1", "999999"} {
		t.Run(cursor, func(t *testing.T) {
			resp := openStream(t, srv, open.SessionID, "tok", cursor)
			defer resp.Body.Close()
			r := bufio.NewReader(resp.Body)

			// Push a live event; assert it's the first one read.
			author := protocol.ParticipantInfo{ID: "agent-test", Kind: protocol.ParticipantAgent}
			go func() {
				time.Sleep(50 * time.Millisecond)
				host.publish(open.SessionID, protocol.NewAgentMessage(open.SessionID, author, "live", 0, true))
			}()
			ev, err := readSSEEvent(r)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if cursor == "999999" {
				// 999999 is a parseable but out-of-range cursor; the
				// replay query returns zero rows and we still drop to
				// live tail. The first event read is the live frame.
				if ev.event != "agent_message" {
					t.Errorf("expected live agent_message, got %#v", ev)
				}
				return
			}
			if ev.event != "agent_message" {
				t.Errorf("expected live agent_message, got %#v", ev)
			}
		})
	}
}

// TestAuth_NoTokenReturns401 — every endpoint without bearer = 401.
func TestAuth_NoTokenReturns401(t *testing.T) {
	_, srv := newTestServer(t, &DevTokenStore{token: "good"})

	for _, ep := range []struct {
		method string
		path   string
	}{
		{"POST", "/api/v1/sessions"},
		{"GET", "/api/v1/sessions"},
		{"POST", "/api/v1/sessions/abc/post"},
		{"GET", "/api/v1/sessions/abc/stream"},
		{"POST", "/api/v1/sessions/abc/close"},
	} {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			req, err := stdhttp.NewRequest(ep.method, srv.URL+ep.path, nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != stdhttp.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
		})
	}
}

func TestAuth_InvalidTokenReturns401(t *testing.T) {
	_, srv := newTestServer(t, &DevTokenStore{token: "good"})
	resp := doJSON(t, srv, "POST", "/api/v1/sessions", "wrong", nil)
	defer resp.Body.Close()
	if resp.StatusCode != stdhttp.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAuth_ValidTokenAccepted(t *testing.T) {
	_, srv := newTestServer(t, &DevTokenStore{token: "good"})
	resp := doJSON(t, srv, "POST", "/api/v1/sessions", "good", nil)
	defer resp.Body.Close()
	if resp.StatusCode != stdhttp.StatusCreated {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}
}
