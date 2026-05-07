package session

import (
	"context"
	"testing"

	"github.com/hugr-lab/hugen/pkg/extension"
)

// stubExtension is a minimal extension that records each
// InitState call. Used to verify Session iterates Deps.Extensions
// and dispatches StateInitializer at session open. Doesn't pull
// any real extension package into pkg/session — keeps the
// dispatch test mechanics-only.
type stubExtension struct {
	name      string
	stateKey  string
	stateVal  any
	initCalls int
}

func (e *stubExtension) Name() string { return e.name }

func (e *stubExtension) InitState(_ context.Context, state extension.SessionState) error {
	e.initCalls++
	if e.stateKey != "" {
		state.SetValue(e.stateKey, e.stateVal)
	}
	return nil
}

// TestExtensionDispatch_InitStateRunsOnce asserts NewSession iterates
// Deps.Extensions and invokes InitState exactly once per registered
// extension. The recorded handle is reachable through SessionState
// after NewSession returns — that's the contract every extension
// migration relies on (notepad, plan, whiteboard, skill, …).
func TestExtensionDispatch_InitStateRunsOnce(t *testing.T) {
	a := &stubExtension{name: "alpha", stateKey: "alpha", stateVal: "α"}
	b := &stubExtension{name: "beta", stateKey: "beta", stateVal: 42}

	parent, cleanup := newTestParent(t, withTestExtensions(a, b))
	defer cleanup()

	if a.initCalls != 1 || b.initCalls != 1 {
		t.Fatalf("InitState calls: alpha=%d beta=%d, want 1 each", a.initCalls, b.initCalls)
	}
	if v, ok := parent.Value("alpha"); !ok || v != "α" {
		t.Errorf("alpha state = %v ok=%v, want α true", v, ok)
	}
	if v, ok := parent.Value("beta"); !ok || v != 42 {
		t.Errorf("beta state = %v ok=%v, want 42 true", v, ok)
	}
}

// TestExtensionDispatch_NoExtensionsIsNoop asserts a session opens
// cleanly with no extensions registered — the iteration loop must
// be safe for empty Deps.Extensions.
func TestExtensionDispatch_NoExtensionsIsNoop(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()
	if _, ok := parent.Value("anything"); ok {
		t.Error("fresh session has unexpected state entry")
	}
}
