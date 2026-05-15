package session

import (
	"context"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// fakeAutocloseExt is a minimal extension implementing
// AutocloseLookup. found+val are returned verbatim so each test
// case can pin the lookup's contract independently.
type fakeAutocloseExt struct {
	val   bool
	found bool
	// lastCall records the (spawnSkill, spawnRole) the lookup
	// was invoked with — lets tests assert the resolver forwards
	// the child's metadata unchanged.
	lastCall struct{ spawnSkill, spawnRole string }
}

func (f *fakeAutocloseExt) Name() string { return "fake-autoclose" }

func (f *fakeAutocloseExt) ResolveAutoclose(_ context.Context, _ extension.SessionState, spawnSkill, spawnRole string) (bool, bool) {
	f.lastCall.spawnSkill = spawnSkill
	f.lastCall.spawnRole = spawnRole
	return f.val, f.found
}

// noopExt is an extension that does NOT implement AutocloseLookup —
// used to verify the resolver skips extensions that opt out of the
// capability.
type noopExt struct{}

func (noopExt) Name() string { return "noop" }

func TestResolveChildAutoclose_Matrix(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		deps  *Deps
		child *Session
		want  bool
	}{
		{
			name: "nil_deps_returns_true",
			deps: nil,
			child: &Session{
				spawnSkill: "data-chat",
				spawnRole:  "data-chatter",
			},
			want: true,
		},
		{
			name: "nil_child_returns_true",
			deps: &Deps{Extensions: []extension.Extension{
				&fakeAutocloseExt{val: false, found: true},
			}},
			child: nil,
			want:  true,
		},
		{
			name: "no_autoclose_lookup_returns_true",
			deps: &Deps{Extensions: []extension.Extension{noopExt{}}},
			child: &Session{
				spawnSkill: "data-chat",
				spawnRole:  "data-chatter",
			},
			want: true,
		},
		{
			name: "lookup_found_false_short_circuits",
			deps: &Deps{Extensions: []extension.Extension{
				&fakeAutocloseExt{val: false, found: true},
			}},
			child: &Session{
				spawnSkill: "data-chat",
				spawnRole:  "data-chatter",
			},
			want: false,
		},
		{
			name: "lookup_found_true_used",
			deps: &Deps{Extensions: []extension.Extension{
				&fakeAutocloseExt{val: true, found: true},
			}},
			child: &Session{spawnSkill: "analyst", spawnRole: "explorer"},
			want:  true,
		},
		{
			name: "lookup_not_found_falls_to_default",
			deps: &Deps{Extensions: []extension.Extension{
				&fakeAutocloseExt{val: false, found: false},
			}},
			child: &Session{spawnSkill: "unknown", spawnRole: "x"},
			want:  true,
		},
		{
			name: "first_extension_with_found_wins",
			deps: &Deps{Extensions: []extension.Extension{
				&fakeAutocloseExt{val: false, found: true},
				&fakeAutocloseExt{val: true, found: true},
			}},
			child: &Session{spawnSkill: "data-chat", spawnRole: "data-chatter"},
			want:  false,
		},
		{
			name: "skips_not_found_extensions",
			deps: &Deps{Extensions: []extension.Extension{
				&fakeAutocloseExt{val: false, found: false},
				&fakeAutocloseExt{val: false, found: true},
			}},
			child: &Session{spawnSkill: "data-chat", spawnRole: "data-chatter"},
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parent := &Session{deps: tc.deps}
			if got := parent.resolveChildAutoclose(ctx, tc.child); got != tc.want {
				t.Errorf("resolveChildAutoclose = %v; want %v", got, tc.want)
			}
		})
	}
}

// TestResolveChildAutoclose_ForwardsSpawnMetadata pins that the
// resolver passes the child's spawnSkill/spawnRole through to the
// lookup unchanged — otherwise the skill extension cannot match
// the manifest entry for the role.
func TestResolveChildAutoclose_ForwardsSpawnMetadata(t *testing.T) {
	ext := &fakeAutocloseExt{val: false, found: true}
	parent := &Session{deps: &Deps{Extensions: []extension.Extension{ext}}}
	child := &Session{spawnSkill: "data-chat", spawnRole: "data-chatter"}
	_ = parent.resolveChildAutoclose(context.Background(), child)
	if ext.lastCall.spawnSkill != "data-chat" || ext.lastCall.spawnRole != "data-chatter" {
		t.Errorf("lookup received (%q, %q); want (data-chat, data-chatter)",
			ext.lastCall.spawnSkill, ext.lastCall.spawnRole)
	}
}

func TestChildIsParked(t *testing.T) {
	cases := []struct {
		name   string
		sess   *Session
		parked bool
	}{
		{"nil", nil, false},
		{"empty_status", &Session{}, false},
		{"idle", &Session{lifecycleState: protocol.SessionStatusIdle}, false},
		{"active", &Session{lifecycleState: protocol.SessionStatusActive}, false},
		{"wait_subagents", &Session{lifecycleState: protocol.SessionStatusWaitSubagents}, false},
		{"awaiting_dismissal", &Session{lifecycleState: protocol.SessionStatusAwaitingDismissal}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := childIsParked(tc.sess); got != tc.parked {
				t.Errorf("childIsParked = %v; want %v", got, tc.parked)
			}
		})
	}
}

// TestIsQuiescent_ParkedChildrenIgnored exercises the phase 5.2
// change to isQuiescent: parked children (awaiting_dismissal) are
// skipped from the live-child count so the parent can transition
// to idle when no active work remains.
func TestIsQuiescent_ParkedChildrenIgnored(t *testing.T) {
	parked := &Session{lifecycleState: protocol.SessionStatusAwaitingDismissal}
	live := &Session{lifecycleState: protocol.SessionStatusActive}

	t.Run("no_children", func(t *testing.T) {
		s := &Session{}
		if !s.isQuiescent() {
			t.Error("empty parent not quiescent; want true")
		}
	})
	t.Run("only_parked_child", func(t *testing.T) {
		s := &Session{children: map[string]*Session{"c1": parked}}
		if !s.isQuiescent() {
			t.Error("parent with only parked children not quiescent; want true")
		}
	})
	t.Run("mixed_children_live_blocks", func(t *testing.T) {
		s := &Session{children: map[string]*Session{
			"c1": parked,
			"c2": live,
		}}
		if s.isQuiescent() {
			t.Error("parent with live child quiescent; want false")
		}
	})
	t.Run("all_live_children_block", func(t *testing.T) {
		s := &Session{children: map[string]*Session{
			"c1": live,
			"c2": live,
		}}
		if s.isQuiescent() {
			t.Error("parent with live child quiescent; want false")
		}
	})
}

// TestSessionStatus_AwaitingDismissalValidates pins the protocol
// validator's acceptance of the new lifecycle state. Without this
// case in the validation switch every emit of SessionStatus with
// state="awaiting_dismissal" would surface as a protocol error.
func TestSessionStatus_AwaitingDismissalValidates(t *testing.T) {
	author := protocol.ParticipantInfo{ID: "a", Kind: protocol.ParticipantAgent, Name: "hugen"}
	frame := protocol.NewSessionStatus("ses-1", author,
		protocol.SessionStatusAwaitingDismissal, "parked_on_result")
	if err := protocol.Validate(frame); err != nil {
		t.Errorf("validate session_status(awaiting_dismissal) = %v; want nil", err)
	}
}

// Phase 5.2 ε — pure-helper coverage for the parking-ceiling /
// idle-timer plumbing. These tests avoid the full Session machinery
// (no store / no Run goroutine) and only exercise the in-memory
// state walks + timer slot. End-to-end behaviour ships in the ι
// scenario harness.

// TestCollectParkedSubtree_GathersAllParkedDescendants pins the
// DFS walk: parked grandchildren and parked direct children both
// surface; non-parked ones are skipped; the newcomer (skip) is
// excluded.
func TestCollectParkedSubtree_GathersAllParkedDescendants(t *testing.T) {
	parked := func(id string) *Session {
		s := &Session{id: id, lifecycleState: protocol.SessionStatusAwaitingDismissal}
		return s
	}
	live := func(id string) *Session {
		return &Session{id: id, lifecycleState: protocol.SessionStatusActive}
	}

	g1 := parked("g1")
	g2 := live("g2")
	g3 := parked("g3")
	c1 := parked("c1")
	c1.children = map[string]*Session{"g1": g1, "g2": g2}
	c2 := live("c2")
	c2.children = map[string]*Session{"g3": g3}
	skip := parked("skip")
	root := &Session{id: "root", children: map[string]*Session{
		"c1": c1, "c2": c2, "skip": skip,
	}}

	got := collectParkedSubtree(root, skip)
	want := map[string]bool{"c1": true, "g1": true, "g3": true}
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d (%v), want %d (%v)", len(got), idsOf(got), len(want), want)
	}
	for _, c := range got {
		if !want[c.id] {
			t.Errorf("unexpected parked id %q in result", c.id)
		}
	}
}

// TestOldestParked_PicksSmallestParkedAt verifies the selector
// returns the session with the earliest parkedAt timestamp.
func TestOldestParked_PicksSmallestParkedAt(t *testing.T) {
	a := &Session{id: "a"}
	a.parkedAt.Store(300)
	b := &Session{id: "b"}
	b.parkedAt.Store(100)
	c := &Session{id: "c"}
	c.parkedAt.Store(200)
	got := oldestParked([]*Session{a, b, c})
	if got == nil || got.id != "b" {
		t.Fatalf("oldestParked = %v; want b", got)
	}
}

// TestOldestParked_EmptyReturnsNil pins the empty-input contract.
func TestOldestParked_EmptyReturnsNil(t *testing.T) {
	if got := oldestParked(nil); got != nil {
		t.Errorf("oldestParked(nil) = %v; want nil", got)
	}
	if got := oldestParked([]*Session{}); got != nil {
		t.Errorf("oldestParked([]) = %v; want nil", got)
	}
}

// TestCancelParkIdleTimer_StopsAndClearsSlot ensures the timer is
// stopped (does not fire) and the slot is cleared after cancel.
func TestCancelParkIdleTimer_StopsAndClearsSlot(t *testing.T) {
	fired := make(chan struct{}, 1)
	child := &Session{id: "c"}
	child.parkTimer = time.AfterFunc(50*time.Millisecond, func() {
		fired <- struct{}{}
	})

	cancelParkIdleTimer(child)

	child.parkTimerMu.Lock()
	if child.parkTimer != nil {
		t.Error("parkTimer slot not cleared after cancel")
	}
	child.parkTimerMu.Unlock()

	select {
	case <-fired:
		t.Error("timer fired after cancel")
	case <-time.After(120 * time.Millisecond):
		// expected — no fire
	}
}

// TestCancelParkIdleTimer_NoTimerIsNoop guards the no-armed-timer
// path — cancelling on a child that never parked must not panic.
func TestCancelParkIdleTimer_NoTimerIsNoop(t *testing.T) {
	cancelParkIdleTimer(nil) // explicit nil receiver tolerated
	child := &Session{id: "c"}
	cancelParkIdleTimer(child)
}

func idsOf(ss []*Session) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.id
	}
	return out
}
