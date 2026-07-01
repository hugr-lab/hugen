package hugenclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// cannedServer stands in for a hugen HTTP API endpoint.
func cannedServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/whoami", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"user_id":"u1","name":"Alice","role":"analyst"}`)
	})
	mux.HandleFunc("POST /v1/sessions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"session_id":"ses-1","status":"idle"}`)
	})
	mux.HandleFunc("GET /v1/sessions", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"id":"ses-1","status":"active"}]`)
	})
	mux.HandleFunc("POST /v1/sessions/{id}/messages", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"status":"accepted"}`)
	})
	mux.HandleFunc("GET /v1/sessions/{id}/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		codec := protocol.NewCodec()
		f := protocol.NewUserMessage("ses-1", protocol.ParticipantInfo{ID: "u1", Kind: protocol.ParticipantUser}, "hello-from-stream")
		data, _ := codec.EncodeFrame(f)
		fmt.Fprintf(w, ": ping\n\n") // heartbeat (ignored)
		fmt.Fprintf(w, "id: 5\ndata: %s\n\n", data)
		if fl != nil {
			fl.Flush()
		}
		<-r.Context().Done()
	})
	mux.HandleFunc("GET /v1/sessions/{id}/boom", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"kaboom"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestClient_WhoAmIAndCreate(t *testing.T) {
	srv := cannedServer(t)
	c := New(srv.URL, WithToken("tok"))
	ctx := context.Background()

	u, err := c.WhoAmI(ctx)
	if err != nil {
		t.Fatalf("WhoAmI: %v", err)
	}
	if u.UserID != "u1" || u.Role != "analyst" {
		t.Errorf("whoami = %+v, want u1/analyst", u)
	}

	id, err := c.CreateSession(ctx, CreateSessionOptions{Name: "t"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if id != "ses-1" {
		t.Errorf("session id = %q, want ses-1", id)
	}

	sessions, err := c.ListSessions(ctx, "")
	if err != nil || len(sessions) != 1 || sessions[0].ID != "ses-1" {
		t.Errorf("ListSessions = %+v, %v", sessions, err)
	}

	if err := c.SendMessage(ctx, "ses-1", "hi"); err != nil {
		t.Errorf("SendMessage: %v", err)
	}
}

func TestClient_Stream(t *testing.T) {
	srv := cannedServer(t)
	c := New(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := c.Stream(ctx, "ses-1", 0)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	select {
	case ev := <-ch:
		if ev.Err != nil {
			t.Fatalf("stream event err: %v", ev.Err)
		}
		if ev.Seq != 5 {
			t.Errorf("seq = %d, want 5", ev.Seq)
		}
		um, ok := ev.Frame.(*protocol.UserMessage)
		if !ok {
			t.Fatalf("frame = %T, want *protocol.UserMessage", ev.Frame)
		}
		if um.Payload.Text != "hello-from-stream" {
			t.Errorf("frame text = %q, want hello-from-stream", um.Payload.Text)
		}
		if um.Seq() != 5 {
			t.Errorf("frame seq stamped = %d, want 5", um.Seq())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no stream event within 2s")
	}
}

func TestClient_APIError(t *testing.T) {
	srv := cannedServer(t)
	c := New(srv.URL)
	err := c.doJSON(context.Background(), http.MethodGet, "/v1/sessions/x/boom", nil, nil)
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("err = %T, want *APIError", err)
	}
	if apiErr.Status != 500 || apiErr.Message != "kaboom" {
		t.Errorf("APIError = %+v, want 500/kaboom", apiErr)
	}
}
