package session

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// permsAllowAll is the default permission service the fixture
// hands to a freshly-built ToolManager when the test doesn't
// supply one. Unconditional allow.
type permsAllowAll struct{}

func (permsAllowAll) Resolve(_ context.Context, _, _ string) (perm.Permission, error) {
	return perm.Permission{}, nil
}
func (permsAllowAll) Refresh(_ context.Context) error { return nil }
func (permsAllowAll) Subscribe(_ context.Context) (<-chan perm.RefreshEvent, error) {
	return nil, nil
}

// drainOutboxOnce reads up to one frame off the outbox or returns
// after a short timeout. Tests use it to swallow lifecycle frames
// (SessionOpened, etc.) before driving the actual scenario.
func drainOutboxOnce(out <-chan protocol.Frame) {
	select {
	case <-out:
	case <-time.After(200 * time.Millisecond):
	}
}

// testParentOpt configures newTestParent.
type testParentOpt func(*testParentCfg)

type testParentCfg struct {
	store      RuntimeStore
	tools      *tool.ToolManager
	sessOpts   []SessionOption
	extensions []extension.Extension
	runLoop    bool
}

// withTestStore sets the RuntimeStore used by the fixture (default:
// newFakeStore()).
func withTestStore(s RuntimeStore) testParentOpt {
	return func(c *testParentCfg) { c.store = s }
}

// withTestTools sets the agent-level (root) ToolManager passed to
// the session constructor. NewSession derives its own per-session
// child off this root.
func withTestTools(tm *tool.ToolManager) testParentOpt {
	return func(c *testParentCfg) { c.tools = tm }
}

// withTestExtensions registers session extensions on the fixture.
// NewSession iterates them and dispatches each to the capability
// hooks it implements (StateInitializer at open, …).
func withTestExtensions(exts ...extension.Extension) testParentOpt {
	return func(c *testParentCfg) {
		c.extensions = append(c.extensions, exts...)
	}
}

// withTestRunLoop starts the parent's Run goroutine. Tests that
// only call tool handlers directly leave it off (default); tests
// whose handler blocks on routeInbound (wait_subagents et al.)
// turn it on so the loop is alive to deliver inbound frames to
// the activeToolFeed slot.
func withTestRunLoop() testParentOpt {
	return func(c *testParentCfg) { c.runLoop = true }
}

// withMissionDispatcher registers a stub extension that implements
// [extension.MissionDispatcher] and answers true for every name in
// `eligible`. spawn_mission catalogue validation tests use this to
// distinguish "skill registered as mission" from "non-existent".
func withMissionDispatcher(eligible ...string) testParentOpt {
	return func(c *testParentCfg) {
		set := map[string]struct{}{}
		for _, n := range eligible {
			set[n] = struct{}{}
		}
		c.extensions = append(c.extensions, &stubMissionDispatcher{eligible: set})
	}
}

// stubMissionDispatcher is a minimal [extension.Extension]
// implementing [extension.MissionDispatcher] for tests. Every
// other capability method is absent — the dispatcher branches in
// spawn_mission only consume MissionSkillExists.
type stubMissionDispatcher struct {
	eligible map[string]struct{}
}

func (s *stubMissionDispatcher) Name() string { return "mission-dispatcher-stub" }

func (s *stubMissionDispatcher) List(context.Context) ([]tool.Tool, error) {
	return nil, nil
}

func (s *stubMissionDispatcher) Call(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}

func (s *stubMissionDispatcher) MissionSkillExists(_ context.Context, name string) (bool, error) {
	_, ok := s.eligible[name]
	return ok, nil
}

// withMissionStartLookup registers a stub extension implementing
// [extension.MissionStartLookup] that returns `block` for every
// matching skill name. Tests use it to verify the on_mission_start
// hook resolves and applies the rendered scaffolding.
func withMissionStartLookup(skill string, block *extension.MissionStartBlock) testParentOpt {
	return func(c *testParentCfg) {
		c.extensions = append(c.extensions, &stubMissionStartLookup{skill: skill, block: block})
	}
}

type stubMissionStartLookup struct {
	skill string
	block *extension.MissionStartBlock
}

func (s *stubMissionStartLookup) Name() string { return "mission-start-stub" }

func (s *stubMissionStartLookup) List(context.Context) ([]tool.Tool, error) {
	return nil, nil
}

func (s *stubMissionStartLookup) Call(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}

func (s *stubMissionStartLookup) ResolveMissionStart(_ context.Context, skill, _ string, _ any) (*extension.MissionStartBlock, error) {
	if skill == s.skill {
		return s.block, nil
	}
	return nil, nil
}

// withPlanSystemWriter and withWhiteboardSystemWriter register
// stub extensions implementing the system-principal write paths.
// They record every call into the stub so tests can assert the
// runtime invoked them in the right order.
func withPlanSystemWriter(stub *stubPlanWriter) testParentOpt {
	return func(c *testParentCfg) { c.extensions = append(c.extensions, stub) }
}

func withWhiteboardSystemWriter(stub *stubWhiteboardWriter) testParentOpt {
	return func(c *testParentCfg) { c.extensions = append(c.extensions, stub) }
}

type stubPlanWriter struct {
	calls []struct{ Text, CurrentStep string }
}

func (s *stubPlanWriter) Name() string { return "plan-writer-stub" }
func (s *stubPlanWriter) List(context.Context) ([]tool.Tool, error) {
	return nil, nil
}
func (s *stubPlanWriter) Call(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}
func (s *stubPlanWriter) SystemSet(_ context.Context, _ extension.SessionState, text, currentStep string) error {
	s.calls = append(s.calls, struct{ Text, CurrentStep string }{text, currentStep})
	return nil
}

type stubWhiteboardWriter struct {
	calls int
}

func (s *stubWhiteboardWriter) Name() string { return "whiteboard-writer-stub" }
func (s *stubWhiteboardWriter) List(context.Context) ([]tool.Tool, error) {
	return nil, nil
}
func (s *stubWhiteboardWriter) Call(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}
func (s *stubWhiteboardWriter) SystemInit(_ context.Context, _ extension.SessionState) error {
	s.calls++
	return nil
}

// newTestParent builds a *Session ready for tool-handler tests
// without spinning up a Manager. Tool tests then call
// `parent.callXxx(ctx, args)` directly — Session implements
// tool.ToolProvider, and the handlers are methods on Session.
//
// Returns: parent session and a cleanup function the test must
// defer (cancels the root ctx and waits for any goroutines spawned
// via parent.Spawn).
func newTestParent(t *testing.T, opts ...testParentOpt) (*Session, func()) {
	t.Helper()
	cfg := &testParentCfg{}
	for _, opt := range opts {
		opt(cfg)
	}
	if cfg.store == nil {
		cfg.store = fixture.NewTestStore()
	}
	if cfg.tools == nil {
		cfg.tools = tool.NewToolManager(permsAllowAll{}, nil, nil)
	}
	mdl := &scriptedModel{}
	router := newRouterWithModel(t, mdl)
	agent, err := NewAgent("a1", "hugen", &fakeIdentity{id: "a1"}, "", nil)
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	rootCtx, cancel := context.WithCancel(context.Background())
	wg := &sync.WaitGroup{}
	deps := &Deps{
		Store:      cfg.store,
		Agent:      agent,
		Models:     router,
		Commands:   NewCommandRegistry(),
		Codec:      protocol.NewCodec(),
		Tools:      cfg.tools,
		Logger:     slog.Default(),
		Opts:       cfg.sessOpts,
		Extensions: cfg.extensions,
		RootCtx:    rootCtx,
		WG:         wg,
		MaxDepth:   DefaultMaxDepth,
	}
	parent, err := New(context.Background(), deps, OpenRequest{OwnerID: "alice"})
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	drainOutboxOnce(parent.Outbox()) // SessionOpened
	if cfg.runLoop {
		parent.Start(context.Background())
	}
	cleanup := func() {
		cancel()
		wg.Wait()
	}
	return parent, cleanup
}

// kindsOnly is a debug helper for failing assertions — extracts the
// sequence of event kinds for a more readable error message.
func kindsOnly(rows []EventRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.EventType)
	}
	return out
}
