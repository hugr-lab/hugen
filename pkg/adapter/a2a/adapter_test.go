package a2a

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/session/manager"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestAdapter_Name(t *testing.T) {
	if got := New().Name(); got != "a2a" {
		t.Fatalf("Name() = %q, want %q", got, "a2a")
	}
}

func TestBuildCard(t *testing.T) {
	a := New(WithBaseURL("https://agent.example.com"))
	card := a.buildCard()

	if card.Name != defaultAgentName {
		t.Errorf("card.Name = %q, want %q", card.Name, defaultAgentName)
	}
	if card.Version == "" {
		t.Error("card.Version is empty; strict clients require a non-empty agent version")
	}
	if len(card.Skills) != 1 || card.Skills[0].ID != defaultSkillID {
		t.Fatalf("card.Skills = %+v, want one skill id %q", card.Skills, defaultSkillID)
	}
	if !card.Capabilities.Streaming || !card.Capabilities.PushNotifications {
		t.Errorf("card.Capabilities = %+v, want streaming+push", card.Capabilities)
	}
	if len(card.SupportedInterfaces) != 1 {
		t.Fatalf("card.SupportedInterfaces = %+v, want exactly one", card.SupportedInterfaces)
	}
	iface := card.SupportedInterfaces[0]
	if want := "https://agent.example.com" + jsonRPCPath; iface.URL != want {
		t.Errorf("interface URL = %q, want %q", iface.URL, want)
	}
	if iface.ProtocolBinding != a2a.TransportProtocolJSONRPC {
		t.Errorf("interface binding = %q, want %q", iface.ProtocolBinding, a2a.TransportProtocolJSONRPC)
	}
	if iface.ProtocolVersion != a2a.Version {
		t.Errorf("interface version = %q, want %q", iface.ProtocolVersion, a2a.Version)
	}
}

func TestBuildCard_IdentityOverride(t *testing.T) {
	a := New(WithAgentIdentity("acme-bot", "does acme things"))
	card := a.buildCard()
	if card.Name != "acme-bot" || card.Description != "does acme things" {
		t.Fatalf("override not applied: name=%q desc=%q", card.Name, card.Description)
	}
}

func TestMessageText(t *testing.T) {
	if got := messageText(nil); got != "" {
		t.Errorf("messageText(nil) = %q, want empty", got)
	}
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello "), a2a.NewTextPart("world"))
	if got := messageText(m); got != "hello world" {
		t.Errorf("messageText = %q, want %q", got, "hello world")
	}
}

func collect(t *testing.T, seq func(func(a2a.Event, error) bool)) []a2a.Event {
	t.Helper()
	var got []a2a.Event
	seq(func(ev a2a.Event, err error) bool {
		if err != nil {
			t.Fatalf("executor yielded error: %v", err)
		}
		got = append(got, ev)
		return true
	})
	return got
}

func TestEchoExecutor_Execute(t *testing.T) {
	e := newEchoExecutor(quietLogger())
	execCtx := &a2asrv.ExecutorContext{
		Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("ping")),
		ContextID: "ctx-1",
		TaskID:    a2a.NewTaskID(),
	}
	events := collect(t, e.Execute(context.Background(), execCtx))
	if len(events) != 1 {
		t.Fatalf("Execute yielded %d events, want 1", len(events))
	}
	msg, ok := events[0].(*a2a.Message)
	if !ok {
		t.Fatalf("event 0 is %T, want *a2a.Message", events[0])
	}
	if got := messageText(msg); got != "echo: ping" {
		t.Errorf("echo reply = %q, want %q", got, "echo: ping")
	}
	if msg.Role != a2a.MessageRoleAgent {
		t.Errorf("reply role = %q, want %q", msg.Role, a2a.MessageRoleAgent)
	}
}

func TestEchoExecutor_Cancel(t *testing.T) {
	e := newEchoExecutor(quietLogger())
	execCtx := &a2asrv.ExecutorContext{ContextID: "ctx-1", TaskID: a2a.NewTaskID()}
	events := collect(t, e.Cancel(context.Background(), execCtx))
	if len(events) != 1 {
		t.Fatalf("Cancel yielded %d events, want 1", len(events))
	}
	upd, ok := events[0].(*a2a.TaskStatusUpdateEvent)
	if !ok {
		t.Fatalf("event 0 is %T, want *a2a.TaskStatusUpdateEvent", events[0])
	}
	if upd.Status.State != a2a.TaskStateCanceled {
		t.Errorf("cancel state = %q, want %q", upd.Status.State, a2a.TaskStateCanceled)
	}
}

// getCard fetches and decodes the agent card from base+path.
func getCard(t *testing.T, url string) *a2a.AgentCard {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d, want 200", url, resp.StatusCode)
	}
	var card a2a.AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		t.Fatalf("decode card from %s: %v", url, err)
	}
	return &card
}

func TestRun_SharedMux_ServesCardAtBothPaths(t *testing.T) {
	mux := http.NewServeMux()
	a := New(WithLogger(quietLogger()), WithBaseURL("http://x"), WithSharedMux(mux))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, stubHost{}) }()

	// The mux is registered synchronously inside Run before it blocks on
	// ctx; a tiny settle keeps the test robust without racing the goroutine.
	srv := httptest.NewServer(mux)
	defer srv.Close()
	waitFor(t, func() bool { return cardReachable(srv.URL + a2asrv.WellKnownAgentCardPath) })

	for _, p := range []string{a2asrv.WellKnownAgentCardPath, legacyCardPath} {
		card := getCard(t, srv.URL+p)
		if card.Name != defaultAgentName {
			t.Errorf("card at %s: name = %q, want %q", p, card.Name, defaultAgentName)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil on ctx cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of ctx cancel")
	}
}

func TestRun_DedicatedListener_Lifecycle(t *testing.T) {
	port := freePort(t)
	a := New(WithLogger(quietLogger()), WithListenPort(port))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, stubHost{}) }()

	base := "http://127.0.0.1:" + itoa(port)
	waitFor(t, func() bool { return cardReachable(base + a2asrv.WellKnownAgentCardPath) })

	card := getCard(t, base+a2asrv.WellKnownAgentCardPath)
	if card.Name != defaultAgentName {
		t.Errorf("dedicated card name = %q, want %q", card.Name, defaultAgentName)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil on ctx cancel", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return within 3s of ctx cancel")
	}
}

// --- test helpers ---

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func cardReachable(url string) bool {
	resp, err := http.Get(url)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// stubHost is a no-op manager.AdapterHost. The A1 echo executor never calls
// host methods (it sets its own logger), so every method returns zero values.
type stubHost struct{}

var _ manager.AdapterHost = stubHost{}

func (stubHost) OpenSession(context.Context, session.OpenRequest) (*session.Session, time.Time, error) {
	return nil, time.Time{}, nil
}
func (stubHost) ResumeSession(context.Context, string) (*session.Session, error) { return nil, nil }
func (stubHost) Submit(context.Context, protocol.Frame) error                    { return nil }
func (stubHost) Subscribe(context.Context, string) (<-chan protocol.Frame, error) {
	return nil, nil
}
func (stubHost) CloseSession(context.Context, string, string) (time.Time, error) {
	return time.Time{}, nil
}
func (stubHost) ListSessions(context.Context, string) ([]session.SessionSummary, error) {
	return nil, nil
}
func (stubHost) SessionStats(context.Context, string) (int, error) { return 0, nil }
func (stubHost) ListEvents(context.Context, string, store.ListEventsOpts) ([]store.EventRow, error) {
	return nil, nil
}
func (stubHost) Logger() *slog.Logger { return quietLogger() }
