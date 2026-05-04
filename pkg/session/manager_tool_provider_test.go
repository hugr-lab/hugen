package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hugr-lab/hugen/pkg/tool"
)

// TestManager_ToolProvider_Identity verifies the static contract:
// Name = "session", Lifetime = per_agent. These are fixed by spec
// (phase-4-architecture §2) and load-bearing for tool routing.
func TestManager_ToolProvider_Identity(t *testing.T) {
	mgr := newTestManager(t, newFakeStore())
	defer mgr.ShutdownAll(context.Background())

	if got := mgr.Name(); got != "session" {
		t.Errorf("Name() = %q, want %q", got, "session")
	}
	if got := mgr.Lifetime(); got != tool.LifetimePerAgent {
		t.Errorf("Lifetime() = %v, want LifetimePerAgent", got)
	}
}

// TestManager_ToolProvider_EmptyDispatch verifies the C7 baseline:
// the dispatch table is empty, so List returns nil and Call returns
// ErrUnknownTool for any name. Per-tool methods land in C10+.
func TestManager_ToolProvider_EmptyDispatch(t *testing.T) {
	if len(sessionTools) != 0 {
		t.Skipf("dispatch table populated (size=%d) — phase-4 step 7+ has landed", len(sessionTools))
	}
	mgr := newTestManager(t, newFakeStore())
	defer mgr.ShutdownAll(context.Background())

	tools, err := mgr.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tools) != 0 {
		t.Errorf("List() len = %d, want 0 (empty dispatch table)", len(tools))
	}

	_, err = mgr.Call(context.Background(), "session:nope", json.RawMessage(`{}`))
	if !errors.Is(err, tool.ErrUnknownTool) {
		t.Errorf("Call(unknown) err = %v, want ErrUnknownTool", err)
	}

	// Subscribe + Close are nil/nil and nil respectively.
	ch, err := mgr.Subscribe(context.Background())
	if err != nil || ch != nil {
		t.Errorf("Subscribe = (%v, %v), want (nil, nil)", ch, err)
	}
	if err := mgr.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
}
