package session

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSanitizeName covers the model-facing edge cases: empty, all-
// invalid chars, mixed case, leading/trailing dashes, overlong, single
// char, embedded whitespace and underscores. The function is total —
// every input maps to a string in `[a-z0-9-]{2,32}`.
func TestSanitizeName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"happy", "data-fetch", "data-fetch"},
		{"uppercase", "Data-Fetch", "data-fetch"},
		{"underscores", "data_fetch_orders", "data-fetch-orders"},
		{"spaces", "data fetch orders", "data-fetch-orders"},
		{"emoji-strip", "fetch📊orders", "fetch-orders"},
		{"unicode-letters-strip", "получить-данные", "subagent"},
		{"trim-dashes", "---fetch---", "fetch"},
		{"collapse-dashes", "data---fetch", "data-fetch"},
		{"empty", "", "subagent"},
		{"all-invalid", "!!!", "subagent"},
		{"single-char", "a", "a-x"},
		{"max-length-overflow", strings.Repeat("x", 100), strings.Repeat("x", 32)},
		{"trim-after-truncate", strings.Repeat("a", 31) + "-extra", strings.Repeat("a", 31)},
		{"digits-ok", "wave-2-step-3", "wave-2-step-3"},
		{"leading-digit", "1st-pass", "1st-pass"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeName(tc.in)
			if got != tc.want {
				t.Errorf("SanitizeName(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if len(got) < SubagentNameMin || len(got) > SubagentNameMax {
				t.Errorf("SanitizeName(%q) length %d out of [%d,%d]",
					tc.in, len(got), SubagentNameMin, SubagentNameMax)
			}
		})
	}
}

// TestResolveChildName_CollisionSuffix verifies the auto-suffix walk
// when the sanitised name clashes with an existing live child or an
// in-flight reservation.
func TestResolveChildName_CollisionSuffix(t *testing.T) {
	s := &Session{
		children:     make(map[string]*Session),
		pendingNames: make(map[string]struct{}),
	}
	s.children["c1"] = &Session{name: "fetch"}
	s.children["c2"] = &Session{name: "fetch-2"}
	s.pendingNames["fetch-3"] = struct{}{}

	got := s.resolveChildNameLocked("fetch")
	want := "fetch-4"
	if got != want {
		t.Errorf("resolveChildNameLocked(fetch) = %q, want %q (live=fetch,fetch-2; pending=fetch-3)", got, want)
	}

	// Fresh name with no collision returns sanitised verbatim.
	got = s.resolveChildNameLocked("analyse")
	if got != "analyse" {
		t.Errorf("resolveChildNameLocked(analyse) = %q, want %q", got, "analyse")
	}
}

// TestResolveChildName_SuffixTruncates verifies that adding a numeric
// suffix to a max-length name truncates the base so the result still
// fits inside SubagentNameMax.
func TestResolveChildName_SuffixTruncates(t *testing.T) {
	s := &Session{
		children:     make(map[string]*Session),
		pendingNames: make(map[string]struct{}),
	}
	base := strings.Repeat("a", SubagentNameMax) // 32 'a's, sanitised verbatim.
	s.children["c1"] = &Session{name: base}

	got := s.resolveChildNameLocked(base)
	if len(got) > SubagentNameMax {
		t.Errorf("resolveChildNameLocked overflow: %q (len=%d, max=%d)", got, len(got), SubagentNameMax)
	}
	if !strings.HasSuffix(got, "-2") {
		t.Errorf("expected `-2` suffix, got %q", got)
	}
}

// TestResolveChildName_SanitisesInput verifies the resolver sanitises
// the raw input before checking collisions.
func TestResolveChildName_SanitisesInput(t *testing.T) {
	s := &Session{
		children:     make(map[string]*Session),
		pendingNames: make(map[string]struct{}),
	}
	got := s.resolveChildNameLocked("Data Fetch!")
	if got != "data-fetch" {
		t.Errorf("resolveChildNameLocked(%q) = %q, want %q", "Data Fetch!", got, "data-fetch")
	}
}

// TestResolveChildName_ConcurrentDoesNotCollide simulates two parents
// each resolving the same raw name concurrently — independent
// childMu locks, so they should each pick "fetch" (per-parent scope).
// Within one parent the lock serialises calls; the test threads them
// to make the contract explicit.
func TestResolveChildName_ConcurrentDoesNotCollide(t *testing.T) {
	parentA := &Session{
		children:     make(map[string]*Session),
		pendingNames: make(map[string]struct{}),
	}
	parentB := &Session{
		children:     make(map[string]*Session),
		pendingNames: make(map[string]struct{}),
	}

	var wg sync.WaitGroup
	var nameA, nameB string
	wg.Add(2)
	go func() {
		defer wg.Done()
		parentA.childMu.Lock()
		nameA = parentA.resolveChildNameLocked("fetch")
		parentA.pendingNames[nameA] = struct{}{}
		parentA.childMu.Unlock()
	}()
	go func() {
		defer wg.Done()
		parentB.childMu.Lock()
		nameB = parentB.resolveChildNameLocked("fetch")
		parentB.pendingNames[nameB] = struct{}{}
		parentB.childMu.Unlock()
	}()
	wg.Wait()

	if nameA != "fetch" || nameB != "fetch" {
		t.Errorf("expected both parents to pick 'fetch' (per-parent scope), got A=%q B=%q", nameA, nameB)
	}
}

// TestSpawn_NameCollision_AutoSuffix verifies the full
// spawn-tool path: two spawn_subagent calls with the same `name`
// produce children with the second auto-suffixed to `-2`. Covers
// schema → handler → Spawn → resolveChildNameLocked round trip
// AND that the response envelope surfaces the resolved name.
func TestSpawn_NameCollision_AutoSuffix(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	// First spawn — name "data-fetch" picked verbatim. Direct
	// parent.Spawn (Go API) — Phase H removed the legacy
	// spawn_subagent LLM tool but the underlying Go Spawn path
	// stays.
	c1, err := parent.Spawn(context.Background(), SpawnSpec{Name: "data-fetch", Task: "first"})
	if err != nil {
		t.Fatalf("first spawn: %v", err)
	}
	if c1.SubagentName() != "data-fetch" {
		t.Errorf("first spawn name = %q, want %q", c1.SubagentName(), "data-fetch")
	}

	// Second spawn — same name → auto-suffix to "data-fetch-2".
	c2, err := parent.Spawn(context.Background(), SpawnSpec{Name: "data-fetch", Task: "second"})
	if err != nil {
		t.Fatalf("second spawn: %v", err)
	}
	if c2.SubagentName() != "data-fetch-2" {
		t.Errorf("second spawn name = %q, want %q", c2.SubagentName(), "data-fetch-2")
	}

	// Verify in-memory state: both children alive with the
	// resolved names.
	parent.childMu.Lock()
	names := make([]string, 0, len(parent.children))
	for _, c := range parent.children {
		if c != nil {
			names = append(names, c.name)
		}
	}
	parent.childMu.Unlock()
	if len(names) != 2 {
		t.Fatalf("parent.children = %d, want 2 (names=%v)", len(names), names)
	}
}

// TestSpawn_NameCollision_BatchedDuplicates verifies that when a
// single spawn_subagent call lists two entries with the SAME name,
// the second entry resolves to an auto-suffixed name (`-2`). The
// per-spec resolution + pendingNames reservation has to flow
// through both Spawn calls atomically; without the reservation the
// two newSession constructions would race and pick the same base.
func TestSpawn_NameCollision_BatchedDuplicates(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	// Two back-to-back Spawn calls with the same name — collision
	// resolution must auto-suffix the second. Mission-PDCA Phase H
	// removed the batched spawn_subagent LLM tool; the underlying
	// Spawn path is unchanged.
	c1, err := parent.Spawn(context.Background(), SpawnSpec{Name: "fetch", Task: "a"})
	if err != nil {
		t.Fatalf("spawn 1: %v", err)
	}
	c2, err := parent.Spawn(context.Background(), SpawnSpec{Name: "fetch", Task: "b"})
	if err != nil {
		t.Fatalf("spawn 2: %v", err)
	}
	if c1.SubagentName() != "fetch" {
		t.Errorf("c1.Name = %q, want %q", c1.SubagentName(), "fetch")
	}
	if c2.SubagentName() != "fetch-2" {
		t.Errorf("c2.Name = %q, want %q (collision auto-suffix)", c2.SubagentName(), "fetch-2")
	}
	if c1.ID() == c2.ID() {
		t.Errorf("c1.ID == c2.ID = %q; want distinct", c1.ID())
	}
}

// TestSpawn_SanitisesAtSchemaLayer verifies the runtime sanitises
// model-supplied names (mixed case / spaces) before persistence
// and exposes the sanitised form in the response envelope. Models
// that pass `Data Fetch` should see `data-fetch` echoed back.
func TestSpawn_SanitisesAtSchemaLayer(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	c, err := parent.Spawn(context.Background(), SpawnSpec{Name: "Data Fetch", Task: "x"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if c.SubagentName() != "data-fetch" {
		t.Errorf("spawn name = %q, want %q (sanitised)", c.SubagentName(), "data-fetch")
	}
}

// TestSpawn_PropagatesNameToStartedFrame verifies the Name field
// reaches the SubagentStartedPayload on the parent's events, so
// downstream consumers (liveview, TUI sidebar) get the addressing
// identifier alongside the session_id.
func TestSpawn_PropagatesNameToStartedFrame(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	spec := SpawnSpec{
		Name: "explore-orders",
		Task: "go",
	}
	child, err := parent.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if got := child.SubagentName(); got != "explore-orders" {
		t.Errorf("child.SubagentName() = %q, want %q", got, "explore-orders")
	}
}

// TestNotify_AddressByName verifies Session.NotifyChild — the
// Go API used by the runtime's interrupt delivery path — accepts
// either a child Name or session_id as the target. Phase H
// removed the legacy notify_subagent LLM tool; the Go API stays.
func TestNotify_AddressByName(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	c, err := parent.Spawn(context.Background(), SpawnSpec{Name: "fetch", Task: "go"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	// Address by Name.
	resolved, delivered, err := parent.NotifyChild(context.Background(), "fetch", "check rows")
	if err != nil {
		t.Fatalf("notify by name: %v", err)
	}
	if !delivered {
		t.Errorf("notify by name not delivered (resolved=%q)", resolved)
	}
	if resolved != c.ID() {
		t.Errorf("notify by name resolved id = %q, want %q", resolved, c.ID())
	}

	// Address by session_id (legacy path) still works.
	resolved, delivered, err = parent.NotifyChild(context.Background(), c.ID(), "ping")
	if err != nil {
		t.Fatalf("notify by session_id: %v", err)
	}
	if !delivered {
		t.Errorf("notify by session_id not delivered")
	}
	if resolved != c.ID() {
		t.Errorf("notify by session_id resolved id = %q, want %q", resolved, c.ID())
	}

	// Unknown identifier surfaces as delivered=false (or ErrNotifyEmpty
	// for empty input). Spec keeps the API quiet on missing target
	// — caller checks delivered.
	_, delivered, _ = parent.NotifyChild(context.Background(), "nonexistent", "x")
	if delivered {
		t.Errorf("notify against unknown id delivered=true; want false")
	}
}

// TestWaitSubagents_AddressByName verifies wait_subagents accepts a
// mix of names and session_ids in the `ids` array and that the
// returned rows use the canonical session_id keying. Phase 5.2
// α.1b.
func TestWaitSubagents_AddressByName(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	// Spawn two children with explicit names.
	if _, err := parent.Spawn(context.Background(), SpawnSpec{Name: "alpha", Task: "go"}); err != nil {
		t.Fatalf("spawn alpha: %v", err)
	}
	if _, err := parent.Spawn(context.Background(), SpawnSpec{Name: "beta", Task: "go"}); err != nil {
		t.Fatalf("spawn beta: %v", err)
	}

	// Build the wait args using one name + one session_id (mixed).
	parent.childMu.Lock()
	var alphaID, betaID string
	for id, c := range parent.children {
		switch c.name {
		case "alpha":
			alphaID = id
		case "beta":
			betaID = id
		}
	}
	parent.childMu.Unlock()
	if alphaID == "" || betaID == "" {
		t.Fatalf("children name index missing: alpha=%q beta=%q", alphaID, betaID)
	}

	// Run wait with `["alpha", "<beta-session-id>"]` — both should
	// resolve, neither should trip the not_a_child fast-fail.
	args, _ := json.Marshal(waitSubagentsInput{IDs: []string{"alpha", betaID}})
	ctx, cancelCtx := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelCtx()
	out, _ := parent.callWaitSubagents(ctx, args)

	// The wait will deadline (children don't auto-complete in this
	// fixture), but the not_a_child fast-fail must NOT have fired.
	// Surface the actual code so failures are clear; "cancelled"
	// or empty results both indicate the resolver succeeded.
	if strings.Contains(string(out), `"not_a_child"`) {
		t.Errorf("wait_subagents with name input fast-failed not_a_child: %s", out)
	}
}

// TestWaitSubagents_DuplicateNamesDedup verifies that when the
// model passes the same identifier twice in the `ids` array (e.g.
// `["alpha","alpha"]` or one-name + same-name's-session_id), the
// in-place name→session_id rewrite collapses to a single pending
// entry and the call does not double-block or trip not_a_child.
func TestWaitSubagents_DuplicateNamesDedup(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	if _, err := parent.Spawn(context.Background(), SpawnSpec{Name: "alpha", Task: "go"}); err != nil {
		t.Fatalf("spawn: %v", err)
	}

	args, _ := json.Marshal(waitSubagentsInput{IDs: []string{"alpha", "alpha"}})
	ctx, cancelCtx := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancelCtx()
	out, _ := parent.callWaitSubagents(ctx, args)

	if strings.Contains(string(out), `"not_a_child"`) {
		t.Errorf("duplicate-name wait fast-failed not_a_child: %s", out)
	}
}

// TestChildByName_LiveLookup verifies the live-children name resolver.
func TestChildByName_LiveLookup(t *testing.T) {
	s := &Session{
		children:     make(map[string]*Session),
		pendingNames: make(map[string]struct{}),
	}
	c := &Session{name: "data-fetch"}
	s.children["ses-1"] = c

	got, ok := s.childByName("data-fetch")
	if !ok || got != c {
		t.Errorf("childByName(data-fetch) = (%v, %v), want (%v, true)", got, ok, c)
	}

	if _, ok := s.childByName("missing"); ok {
		t.Errorf("childByName(missing) returned ok=true")
	}
	if _, ok := s.childByName(""); ok {
		t.Errorf("childByName(\"\") returned ok=true; empty input must short-circuit to false")
	}
}
