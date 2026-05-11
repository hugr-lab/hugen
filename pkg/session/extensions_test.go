package session

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
	"github.com/hugr-lab/hugen/pkg/tool"
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

// fullCapStubExtension implements every capability the four loops
// dispatch. Each capability records a single call so a test can
// assert the loop fired exactly once.
type fullCapStubExtension struct {
	name string

	recoverCalls    int
	recoverRowCount int
	closeCalls      int
	advertiseCalls  int
	advertiseText   string
	filterCalls     int
	filterDrop      string // names containing this substring are filtered out
	gen             int64
}

func (e *fullCapStubExtension) Name() string { return e.name }

func (e *fullCapStubExtension) Recover(_ context.Context, _ extension.SessionState, rows []store.EventRow) error {
	e.recoverCalls++
	e.recoverRowCount = len(rows)
	return nil
}

func (e *fullCapStubExtension) CloseSession(_ context.Context, _ extension.SessionState) error {
	e.closeCalls++
	return nil
}

func (e *fullCapStubExtension) AdvertiseSystemPrompt(_ context.Context, _ extension.SessionState) string {
	e.advertiseCalls++
	return e.advertiseText
}

func (e *fullCapStubExtension) FilterTools(_ context.Context, _ extension.SessionState, all []tool.Tool) []tool.Tool {
	e.filterCalls++
	if e.filterDrop == "" {
		return all
	}
	out := make([]tool.Tool, 0, len(all))
	for _, t := range all {
		if !strings.Contains(t.Name, e.filterDrop) {
			out = append(out, t)
		}
	}
	return out
}

func (e *fullCapStubExtension) Generation(_ extension.SessionState) int64 { return e.gen }

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

// TestExtensionRecovery_DispatchedFromMaterialise asserts the
// recovery loop in materialise calls every Recovery extension once
// per session. The stub records the row count it was given so the
// test sees the same []EventRow the static walk consumes.
//
// New (open-path) sessions short-circuit materialise — the flag is
// flipped at construction since there's no event log to replay. To
// exercise the loop the test resets matOnce + materialised so the
// next call drives the full body, mirroring newSessionRestore.
func TestExtensionRecovery_DispatchedFromMaterialise(t *testing.T) {
	a := &fullCapStubExtension{name: "alpha"}
	b := &fullCapStubExtension{name: "beta"}

	parent, cleanup := newTestParent(t, withTestExtensions(a, b))
	defer cleanup()

	parent.matOnce = sync.Once{}
	parent.materialised.Store(false)

	if err := parent.materialise(context.Background()); err != nil {
		t.Fatalf("materialise: %v", err)
	}
	if a.recoverCalls != 1 || b.recoverCalls != 1 {
		t.Fatalf("recoverCalls: alpha=%d beta=%d, want 1 each",
			a.recoverCalls, b.recoverCalls)
	}
	// Calling materialise again is a no-op (matOnce).
	if err := parent.materialise(context.Background()); err != nil {
		t.Fatalf("materialise (2nd): %v", err)
	}
	if a.recoverCalls != 1 || b.recoverCalls != 1 {
		t.Errorf("Recovery dispatched twice: alpha=%d beta=%d",
			a.recoverCalls, b.recoverCalls)
	}
}

// TestExtensionCloser_DispatchedReverseOrder asserts the Closer
// dispatch runs in reverse registration order so a later extension
// whose state depends on an earlier one releases first.
func TestExtensionCloser_DispatchedReverseOrder(t *testing.T) {
	var order []string
	a := &recordingCloser{name: "alpha", record: &order}
	b := &recordingCloser{name: "beta", record: &order}
	c := &recordingCloser{name: "gamma", record: &order}

	parent, cleanup := newTestParent(t, withTestExtensions(a, b, c))
	defer cleanup()

	parent.dispatchExtensionClosers(context.Background())
	want := []string{"gamma", "beta", "alpha"}
	if len(order) != len(want) {
		t.Fatalf("order=%v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, order[i], want[i])
		}
	}
}

// TestSystemPrompt_TierHeader asserts the system prompt opens
// with a `Session tier: <tier>` line resolved from the session's
// depth (phase 4.2.2 §9). The root constructor leaves depth 0, so
// the header reads "root"; that's the only tier reachable via
// newTestParent without a real spawn.
func TestSystemPrompt_TierHeader(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()
	body := parent.systemPrompt(context.Background())
	if !strings.HasPrefix(body, "Session tier: root") {
		t.Errorf("systemPrompt prefix missing tier header; got: %q", body)
	}
}

// TestExtensionAdvertiser_AppendsSection asserts non-empty
// Advertiser sections land in the system prompt body in
// registration order, and empty sections are skipped.
func TestExtensionAdvertiser_AppendsSection(t *testing.T) {
	a := &fullCapStubExtension{name: "alpha", advertiseText: "alpha-block"}
	silent := &fullCapStubExtension{name: "silent", advertiseText: ""}
	b := &fullCapStubExtension{name: "beta", advertiseText: "beta-block"}

	parent, cleanup := newTestParent(t, withTestExtensions(a, silent, b))
	defer cleanup()

	body := parent.systemPrompt(context.Background())
	if !strings.Contains(body, "alpha-block") || !strings.Contains(body, "beta-block") {
		t.Fatalf("systemPrompt body missing extension sections: %q", body)
	}
	// silent contributed nothing — body must NOT carry the empty
	// section as a separator pair.
	if a.advertiseCalls != 1 || silent.advertiseCalls != 1 || b.advertiseCalls != 1 {
		t.Errorf("advertise calls: alpha=%d silent=%d beta=%d, want 1 each",
			a.advertiseCalls, silent.advertiseCalls, b.advertiseCalls)
	}
	// Order: alpha must precede beta.
	if i, j := strings.Index(body, "alpha-block"), strings.Index(body, "beta-block"); i > j {
		t.Errorf("alpha section at %d after beta at %d — order wrong", i, j)
	}
}

// TestExtensionToolFilter_NarrowsCatalogue asserts the ToolFilter
// loop composes by intersection over the Snapshot output. Uses a
// tools fixture that surfaces three names; the filter drops the one
// containing "drop".
func TestExtensionToolFilter_NarrowsCatalogue(t *testing.T) {
	provider := &staticToolProvider{
		name: "fixt",
		tools: []tool.Tool{
			{Name: "fixt:keep_a", Provider: "fixt"},
			{Name: "fixt:drop_me", Provider: "fixt"},
			{Name: "fixt:keep_b", Provider: "fixt"},
		},
	}
	tm := tool.NewToolManager(permsAllowAll{}, nil, nil)
	if err := tm.AddProvider(provider); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}

	ext := &fullCapStubExtension{name: "narrow", filterDrop: "drop"}
	parent, cleanup := newTestParent(t, withTestTools(tm), withTestExtensions(ext))
	defer cleanup()

	snap, err := parent.fetchSnapshot(context.Background())
	if err != nil {
		t.Fatalf("fetchSnapshot: %v", err)
	}
	if ext.filterCalls != 1 {
		t.Fatalf("filterCalls=%d, want 1", ext.filterCalls)
	}
	names := make(map[string]bool)
	for _, tl := range snap.Tools {
		names[tl.Name] = true
	}
	if names["fixt:drop_me"] {
		t.Errorf("filter did not drop fixt:drop_me; got %+v", names)
	}
	if !names["fixt:keep_a"] || !names["fixt:keep_b"] {
		t.Errorf("filter dropped too much; got %+v", names)
	}

	// Second fetch reuses the cache — filter not invoked again.
	if _, err := parent.fetchSnapshot(context.Background()); err != nil {
		t.Fatalf("fetchSnapshot (2nd): %v", err)
	}
	if ext.filterCalls != 1 {
		t.Errorf("cache miss after gen unchanged: filterCalls=%d, want 1", ext.filterCalls)
	}

	// Bumping Generation invalidates the cache, filter runs again.
	ext.gen++
	if _, err := parent.fetchSnapshot(context.Background()); err != nil {
		t.Fatalf("fetchSnapshot (3rd): %v", err)
	}
	if ext.filterCalls != 2 {
		t.Errorf("Generation bump did not invalidate cache: filterCalls=%d, want 2",
			ext.filterCalls)
	}
}

// recordingCloser only implements Closer (plus the marker). Used by
// the closer-order test so InitState / Recover / Filter / Advertise
// noise stays out of the recorded order.
type recordingCloser struct {
	name   string
	record *[]string
}

func (e *recordingCloser) Name() string { return e.name }
func (e *recordingCloser) CloseSession(_ context.Context, _ extension.SessionState) error {
	*e.record = append(*e.record, e.name)
	return nil
}

// staticToolProvider is a tool.ToolProvider with a fixed catalogue.
// Used by the ToolFilter test so the snapshot has predictable rows
// to filter against.
type staticToolProvider struct {
	name  string
	tools []tool.Tool
}

func (p *staticToolProvider) Name() string            { return p.name }
func (p *staticToolProvider) Lifetime() tool.Lifetime { return tool.LifetimePerAgent }
func (p *staticToolProvider) List(_ context.Context) ([]tool.Tool, error) {
	return p.tools, nil
}
func (p *staticToolProvider) Call(_ context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}
func (p *staticToolProvider) Subscribe(_ context.Context) (<-chan tool.ProviderEvent, error) {
	return nil, nil
}
func (p *staticToolProvider) Close() error { return nil }

// recordingFrameRouter only implements FrameRouter (plus the marker).
// Used by the dispatcher test so other capabilities don't fire.
type recordingFrameRouter struct {
	name      string
	calls     int
	lastOp    string
	returnErr error
}

func (e *recordingFrameRouter) Name() string { return e.name }
func (e *recordingFrameRouter) HandleFrame(_ context.Context, _ extension.SessionState, f *protocol.ExtensionFrame) error {
	e.calls++
	e.lastOp = f.Payload.Op
	return e.returnErr
}

// TestDispatchExtensionFrame_RoutesToMatchingExtension asserts that an
// inbound ExtensionFrame addressed to a registered extension lands on
// that extension's HandleFrame, while a frame addressed to an unknown
// extension is silently dropped (debug-logged).
func TestDispatchExtensionFrame_RoutesToMatchingExtension(t *testing.T) {
	target := &recordingFrameRouter{name: "target"}
	other := &recordingFrameRouter{name: "other"}

	parent, cleanup := newTestParent(t, withTestExtensions(target, other))
	defer cleanup()

	author := protocol.ParticipantInfo{ID: "agent", Kind: protocol.ParticipantAgent}
	frame := protocol.NewExtensionFrame(parent.ID(), author, "target",
		protocol.CategoryOp, "ping", nil)

	dispatchExtensionFrame(parent, context.Background(), frame)

	if target.calls != 1 {
		t.Errorf("target HandleFrame calls = %d, want 1", target.calls)
	}
	if target.lastOp != "ping" {
		t.Errorf("target last op = %q, want ping", target.lastOp)
	}
	if other.calls != 0 {
		t.Errorf("other HandleFrame calls = %d, want 0", other.calls)
	}

	// Unknown extension name: silently dropped, no router fired.
	unknown := protocol.NewExtensionFrame(parent.ID(), author, "ghost",
		protocol.CategoryOp, "noop", nil)
	dispatchExtensionFrame(parent, context.Background(), unknown)
	if target.calls != 1 || other.calls != 0 {
		t.Errorf("unknown extension fired a router; target=%d other=%d",
			target.calls, other.calls)
	}
}
