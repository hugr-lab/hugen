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
	// Two interfaces at the same /a2a URL: v1.0 + v0.3 (header-dispatched).
	if len(card.SupportedInterfaces) != 2 {
		t.Fatalf("card.SupportedInterfaces = %+v, want two (v1.0 + v0.3)", card.SupportedInterfaces)
	}
	wantURL := "https://agent.example.com" + jsonRPCPath
	versions := map[a2a.ProtocolVersion]bool{}
	for _, iface := range card.SupportedInterfaces {
		if iface.URL != wantURL {
			t.Errorf("interface URL = %q, want %q", iface.URL, wantURL)
		}
		if iface.ProtocolBinding != a2a.TransportProtocolJSONRPC {
			t.Errorf("interface binding = %q, want %q", iface.ProtocolBinding, a2a.TransportProtocolJSONRPC)
		}
		versions[iface.ProtocolVersion] = true
	}
	if !versions[a2a.Version] || !versions[protocolVersion03] {
		t.Errorf("interface versions = %v, want both %q and %q", versions, a2a.Version, protocolVersion03)
	}
}

func TestAPIKeyGate(t *testing.T) {
	a := New(WithAPIKey("s3cret"))
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	h := a.apiKeyGate(next)
	cases := []struct {
		name string
		key  string
		set  bool
		want int
		pass bool
	}{
		{"missing", "", false, http.StatusUnauthorized, false},
		{"wrong", "nope", true, http.StatusUnauthorized, false},
		{"correct", "s3cret", true, http.StatusOK, true},
	}
	for _, c := range cases {
		called = false
		req := httptest.NewRequest(http.MethodPost, jsonRPCPath, nil)
		if c.set {
			req.Header.Set(apiKeyHeader, c.key)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: status = %d, want %d", c.name, rec.Code, c.want)
		}
		if called != c.pass {
			t.Errorf("%s: reached next = %v, want %v", c.name, called, c.pass)
		}
	}
}

func TestBuildCard_APIKey(t *testing.T) {
	// No key → the card advertises no security (clients connect anonymously).
	open := New().buildCard()
	if len(open.SecuritySchemes) != 0 || len(open.SecurityRequirements) != 0 {
		t.Errorf("open card advertises security: schemes=%v reqs=%v", open.SecuritySchemes, open.SecurityRequirements)
	}
	// Key → the apiKey scheme is declared + required, naming the header.
	c := New(WithAPIKey("k")).buildCard()
	sch, ok := c.SecuritySchemes[apiKeySchemeName]
	if !ok {
		t.Fatalf("card missing the %q scheme: %v", apiKeySchemeName, c.SecuritySchemes)
	}
	ak, ok := sch.(a2a.APIKeySecurityScheme)
	if !ok {
		t.Fatalf("scheme is %T, want a2a.APIKeySecurityScheme", sch)
	}
	if ak.Location != a2a.APIKeySecuritySchemeLocationHeader || ak.Name != apiKeyHeader {
		t.Errorf("scheme = %+v, want location=header name=%s", ak, apiKeyHeader)
	}
	if len(c.SecurityRequirements) != 1 {
		t.Errorf("security requirements = %v, want one (apiKey)", c.SecurityRequirements)
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

// fakeFrameIO is a programmable frameIO for executor tests: Subscribe returns
// a pre-filled channel; Submit records frames.
type fakeFrameIO struct {
	ch        chan protocol.Frame
	submitted []protocol.Frame
	subID     string
	subErr    error
	submitErr error
}

func (f *fakeFrameIO) Subscribe(_ context.Context, sid string) (<-chan protocol.Frame, error) {
	f.subID = sid
	if f.subErr != nil {
		return nil, f.subErr
	}
	return f.ch, nil
}

func (f *fakeFrameIO) Submit(_ context.Context, fr protocol.Frame) error {
	if f.submitErr != nil {
		return f.submitErr
	}
	f.submitted = append(f.submitted, fr)
	return nil
}

// collectErr runs the iterator and returns the events plus the first error.
func collectErr(seq func(func(a2a.Event, error) bool)) ([]a2a.Event, error) {
	var got []a2a.Event
	var firstErr error
	seq(func(ev a2a.Event, err error) bool {
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if ev != nil {
			got = append(got, ev)
		}
		return true
	})
	return got, firstErr
}

// agentFrame builds a LIVE streaming chunk (Consolidated=false) — the shape
// the executor accumulates (mirrors the runtime's outbox).
func agentFrame(root, text string, final bool, seq int) protocol.Frame {
	return protocol.NewAgentMessage(root, serviceParticipant(), text, seq, final)
}

func idleFrame(root, reason string) protocol.Frame {
	return protocol.NewSessionStatus(root, serviceParticipant(), protocol.SessionStatusIdle, reason)
}

func TestSessionExecutor_Execute_SyncTurn(t *testing.T) {
	io := &fakeFrameIO{ch: make(chan protocol.Frame, 8)}
	// fakeRootStore opens "root-1" for the first contextId. Pre-fill: the
	// open-time idle(session_opened) (must be SKIPPED, not treated as a
	// boundary), two live chunks, then the real turn boundary.
	io.ch <- idleFrame("root-1", "session_opened")
	io.ch <- agentFrame("root-1", "Hello ", false, 0)
	io.ch <- agentFrame("root-1", "there.", true, 1)
	io.ch <- idleFrame("root-1", "turn_complete")

	reg := newContextRegistry(&fakeRootStore{}, quietLogger())
	e := newSessionExecutor(quietLogger(), reg, io, serviceParticipant(), nil)
	execCtx := &a2asrv.ExecutorContext{
		Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi")),
		ContextID: "ctx-1",
		TaskID:    a2a.NewTaskID(),
	}

	events, err := collectErr(e.Execute(context.Background(), execCtx))
	if err != nil {
		t.Fatalf("Execute yielded error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("Execute yielded %d events, want 1 (final message)", len(events))
	}
	msg, ok := events[0].(*a2a.Message)
	if !ok {
		t.Fatalf("event 0 is %T, want *a2a.Message", events[0])
	}
	if got := messageText(msg); got != "Hello there." {
		t.Errorf("reply = %q, want %q (accumulated consolidated text)", got, "Hello there.")
	}

	// The inbound was submitted as a user_message addressed at the root.
	if len(io.submitted) != 1 {
		t.Fatalf("submitted %d frames, want 1", len(io.submitted))
	}
	um, ok := io.submitted[0].(*protocol.UserMessage)
	if !ok {
		t.Fatalf("submitted[0] is %T, want *protocol.UserMessage", io.submitted[0])
	}
	if um.SessionID() != "root-1" {
		t.Errorf("user_message session = %q, want root-1", um.SessionID())
	}
	if um.Payload.Text != "hi" {
		t.Errorf("user_message text = %q, want hi", um.Payload.Text)
	}
	if io.subID != "root-1" {
		t.Errorf("subscribed to %q, want root-1", io.subID)
	}
}

func TestSessionExecutor_Execute_ErrorFrame(t *testing.T) {
	io := &fakeFrameIO{ch: make(chan protocol.Frame, 4)}
	io.ch <- protocol.NewError("root-1", serviceParticipant(), "boom", "kaboom", false)

	reg := newContextRegistry(&fakeRootStore{}, quietLogger())
	e := newSessionExecutor(quietLogger(), reg, io, serviceParticipant(), nil)
	execCtx := &a2asrv.ExecutorContext{
		Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi")),
		ContextID: "ctx-1",
		TaskID:    a2a.NewTaskID(),
	}

	events, err := collectErr(e.Execute(context.Background(), execCtx))
	if err == nil {
		t.Fatal("Execute did not yield an error for an error frame")
	}
	if len(events) != 0 {
		t.Errorf("error turn yielded %d events, want 0", len(events))
	}
}

func TestSessionExecutor_Execute_CtxCancel(t *testing.T) {
	io := &fakeFrameIO{ch: make(chan protocol.Frame, 4)}
	io.ch <- agentFrame("root-1", "partial", true, 0) // no idle boundary follows

	reg := newContextRegistry(&fakeRootStore{}, quietLogger())
	e := newSessionExecutor(quietLogger(), reg, io, serviceParticipant(), nil)
	execCtx := &a2asrv.ExecutorContext{
		Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi")),
		ContextID: "ctx-1",
		TaskID:    a2a.NewTaskID(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled → drain must not hang waiting for idle

	events, err := collectErr(e.Execute(ctx, execCtx))
	if err != nil {
		t.Fatalf("ctx-cancel turn yielded error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("ctx-cancel turn yielded %d events, want 1 (partial reply)", len(events))
	}
	if _, ok := events[0].(*a2a.Message); !ok {
		t.Fatalf("event 0 is %T, want *a2a.Message", events[0])
	}
}

func TestVersionDispatch(t *testing.T) {
	mark := func(s string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, s) })
	}
	h := versionDispatchHandler{v1: mark("v1"), v03: mark("v03")}
	cases := []struct{ hdr, want string }{
		{"", "v03"},    // Copilot: no version header → v0.3
		{"0.3", "v03"}, // explicit v0.3
		{"1.0", "v1"},  // spec-compliant v1.0
		{"1.4", "v1"},  // any 1.x
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, jsonRPCPath, nil)
		if c.hdr != "" {
			req.Header.Set("A2A-Version", c.hdr)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := rec.Body.String(); got != c.want {
			t.Errorf("A2A-Version %q → %q, want %q", c.hdr, got, c.want)
		}
	}
}

// copilotHistory builds the nested chathistory metadata envelope the way it
// arrives after JSON decode (verified MS-sample shape, §2.3a).
func copilotHistory(entries ...[2]string) map[string]any {
	var vals []any
	for _, e := range entries {
		vals = append(vals, map[string]any{"From": e[0], "Locale": "en-US", "Text": e[1]})
	}
	return map[string]any{
		chatHistoryMetaKey: []any{map[string]any{"HasValue": true, "Value": vals}},
	}
}

func TestLatestUserTextFromHistory(t *testing.T) {
	// From:"" = user; non-empty From = agent. Want the LAST user entry.
	m := &a2a.Message{Metadata: copilotHistory(
		[2]string{"agent1", "Hello"},
		[2]string{"", "first user msg"},
		[2]string{"agent1", "answer"},
		[2]string{"", "latest user msg\n\n"},
	)}
	if got := latestUserTextFromHistory(m); got != "latest user msg" {
		t.Errorf("latestUserTextFromHistory = %q, want %q", got, "latest user msg")
	}
	if got := latestUserTextFromHistory(&a2a.Message{}); got != "" {
		t.Errorf("no-metadata = %q, want empty", got)
	}
	if got := latestUserTextFromHistory(nil); got != "" {
		t.Errorf("nil message = %q, want empty", got)
	}
}

func TestSessionExecutor_Execute_EmptyParts_FallsBackToHistory(t *testing.T) {
	io := &fakeFrameIO{ch: make(chan protocol.Frame, 2)}
	io.ch <- idleFrame("root-1", "turn_complete")

	reg := newContextRegistry(&fakeRootStore{}, quietLogger())
	e := newSessionExecutor(quietLogger(), reg, io, serviceParticipant(), nil)
	// Empty parts; the latest user turn is only in the chathistory tail.
	msg := &a2a.Message{Role: a2a.MessageRoleUser, Metadata: copilotHistory(
		[2]string{"agent1", "Hi there"},
		[2]string{"", "recovered question"},
	)}
	execCtx := &a2asrv.ExecutorContext{Message: msg, ContextID: "ctx-1", TaskID: a2a.NewTaskID()}

	if _, err := collectErr(e.Execute(context.Background(), execCtx)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(io.submitted) != 1 {
		t.Fatalf("submitted %d, want 1", len(io.submitted))
	}
	um := io.submitted[0].(*protocol.UserMessage)
	if um.Payload.Text != "recovered question" {
		t.Errorf("submitted text = %q, want %q (recovered from history)", um.Payload.Text, "recovered question")
	}
}

func TestSessionExecutor_Execute_WarmIgnoresHistory(t *testing.T) {
	io := &fakeFrameIO{ch: make(chan protocol.Frame, 2)}
	io.ch <- idleFrame("root-1", "turn_complete")

	reg := newContextRegistry(&fakeRootStore{}, quietLogger())
	e := newSessionExecutor(quietLogger(), reg, io, serviceParticipant(), nil)
	// parts carries the new turn; chathistory is full replayed context that the
	// durable session already holds — must be IGNORED (no double history).
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("the real new message"))
	msg.Metadata = copilotHistory(
		[2]string{"agent1", "old agent reply"},
		[2]string{"", "an older user message"},
		[2]string{"", "the real new message"},
	)
	execCtx := &a2asrv.ExecutorContext{Message: msg, ContextID: "ctx-1", TaskID: a2a.NewTaskID()}

	if _, err := collectErr(e.Execute(context.Background(), execCtx)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(io.submitted) != 1 {
		t.Fatalf("submitted %d, want 1 (only the new turn, not replayed history)", len(io.submitted))
	}
	um := io.submitted[0].(*protocol.UserMessage)
	if um.Payload.Text != "the real new message" {
		t.Errorf("submitted text = %q, want only the parts text", um.Payload.Text)
	}
}

func TestSessionExecutor_Cancel(t *testing.T) {
	reg := newContextRegistry(&fakeRootStore{}, quietLogger())
	e := newSessionExecutor(quietLogger(), reg, &fakeFrameIO{}, serviceParticipant(), nil)
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
