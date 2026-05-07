package session

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/internal/fixture"
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
	agent, err := NewAgent("a1", "hugen", &fakeIdentity{id: "a1"}, "")
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
