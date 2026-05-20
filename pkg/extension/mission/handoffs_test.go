package mission

import (
	"testing"
	"time"
)

func TestMakeRef(t *testing.T) {
	tests := []struct {
		name      string
		subagent  string
		wave      string
		want      string
		wantError bool
	}{
		{name: "ok", subagent: "schema-orders", wave: "schema-discovery", want: "schema-orders@schema-discovery"},
		{name: "ok with trim", subagent: " a ", wave: " b ", want: "a@b"},
		{name: "empty subagent", wave: "w", wantError: true},
		{name: "empty wave", subagent: "s", wantError: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MakeRef(tc.subagent, tc.wave)
			if tc.wantError {
				if err == nil {
					t.Fatalf("MakeRef(%q,%q): want error, got %q", tc.subagent, tc.wave, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("MakeRef(%q,%q): unexpected error %v", tc.subagent, tc.wave, err)
			}
			if got != tc.want {
				t.Fatalf("MakeRef(%q,%q) = %q, want %q", tc.subagent, tc.wave, got, tc.want)
			}
		})
	}
}

func TestParseRef(t *testing.T) {
	name, wave, err := ParseRef("schema-orders@schema-discovery")
	if err != nil {
		t.Fatalf("ParseRef: unexpected error %v", err)
	}
	if name != "schema-orders" || wave != "schema-discovery" {
		t.Fatalf("ParseRef: got (%q,%q), want (schema-orders, schema-discovery)", name, wave)
	}

	for _, bad := range []string{"", "@", "name@", "@wave", "noatsign", "  "} {
		if _, _, err := ParseRef(bad); err == nil {
			t.Errorf("ParseRef(%q): want error, got nil", bad)
		}
	}
}

func TestHandoffsPutGetList(t *testing.T) {
	store := NewHandoffs()

	if store.Len() != 0 {
		t.Fatalf("fresh store Len = %d, want 0", store.Len())
	}
	if _, ok := store.Get("anything"); ok {
		t.Fatalf("Get on empty store returned ok=true")
	}

	t0 := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	store.Put(Handoff{Ref: "a@w1", Status: "ok", CreatedAt: t0})
	store.Put(Handoff{Ref: "b@w1", Status: "ok", CreatedAt: t0.Add(1 * time.Second)})
	store.Put(Handoff{Ref: "c@w1", Status: "ok", CreatedAt: t0.Add(-1 * time.Second)})

	if got := store.Len(); got != 3 {
		t.Fatalf("Len after 3 puts = %d, want 3", got)
	}

	a, ok := store.Get("a@w1")
	if !ok || a.Status != "ok" {
		t.Fatalf("Get(a@w1): ok=%v status=%q want (true, ok)", ok, a.Status)
	}

	// List is sorted by CreatedAt asc.
	list := store.List()
	if len(list) != 3 {
		t.Fatalf("List len = %d, want 3", len(list))
	}
	if list[0].Ref != "c@w1" || list[1].Ref != "a@w1" || list[2].Ref != "b@w1" {
		t.Fatalf("List order = [%s,%s,%s], want [c,a,b]", list[0].Ref, list[1].Ref, list[2].Ref)
	}
}

func TestHandoffsPutEmptyRefIgnored(t *testing.T) {
	store := NewHandoffs()
	store.Put(Handoff{Ref: "", Status: "ok"})
	if store.Len() != 0 {
		t.Fatalf("Len after empty-ref Put = %d, want 0", store.Len())
	}
}

func TestHandoffsOverwriteOnSameRef(t *testing.T) {
	store := NewHandoffs()
	store.Put(Handoff{Ref: "x@w", Status: "ok", Reason: "first"})
	store.Put(Handoff{Ref: "x@w", Status: "error", Reason: "second"})
	got, ok := store.Get("x@w")
	if !ok {
		t.Fatal("Get after overwrite: not found")
	}
	if got.Status != "error" || got.Reason != "second" {
		t.Fatalf("Get after overwrite: status=%q reason=%q, want (error, second)", got.Status, got.Reason)
	}
}
