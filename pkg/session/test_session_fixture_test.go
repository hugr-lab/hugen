package session

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

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
	store    RuntimeStore
	skills   *skill.SkillManager
	tools    *tool.ToolManager
	perms    perm.Service
	sessOpts []SessionOption
	runLoop  bool
}

// withTestStore sets the RuntimeStore used by the fixture (default:
// newFakeStore()).
func withTestStore(s RuntimeStore) testParentOpt {
	return func(c *testParentCfg) { c.store = s }
}

// withTestSkills attaches a SkillManager to the session via
// WithSkills and records it on the cfg so callers can fetch it
// from the returned host (skill_files passes it through).
func withTestSkills(s *skill.SkillManager) testParentOpt {
	return func(c *testParentCfg) {
		c.skills = s
		c.sessOpts = append(c.sessOpts, WithSkills(s))
	}
}

// withTestTools attaches a ToolManager to the session via
// WithTools.
func withTestTools(tm *tool.ToolManager) testParentOpt {
	return func(c *testParentCfg) {
		c.tools = tm
		c.sessOpts = append(c.sessOpts, WithTools(tm))
	}
}

// withTestPerms attaches a perm.Service to the session via
// WithPerms. Tests that don't exercise the permission gate can
// skip this option.
func withTestPerms(p perm.Service) testParentOpt {
	return func(c *testParentCfg) {
		c.perms = p
		c.sessOpts = append(c.sessOpts, WithPerms(p))
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
		cfg.store = newFakeStore()
	}
	mdl := &scriptedModel{}
	router := newRouterWithModel(t, mdl)
	agent, err := NewAgent("a1", "hugen", &fakeIdentity{id: "a1"}, "")
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	rootCtx, cancel := context.WithCancel(context.Background())
	wg := &sync.WaitGroup{}
	deps := &Deps{
		Store:    cfg.store,
		Agent:    agent,
		Models:   router,
		Commands: NewCommandRegistry(),
		Codec:    protocol.NewCodec(),
		Logger:   slog.Default(),
		Opts:     cfg.sessOpts,
		RootCtx:  rootCtx,
		WG:       wg,
		MaxDepth: DefaultMaxDepth,
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
