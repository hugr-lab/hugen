package main

import (
	"testing"

	"github.com/hugr-lab/hugen/pkg/session"
)

func TestRegisterBuiltinCommands(t *testing.T) {
	reg := session.NewCommandRegistry()
	if err := registerBuiltinCommands(reg, nil); err != nil {
		t.Fatalf("register: %v", err)
	}
	want := []string{"cancel", "end", "help", "model", "note"}
	got := reg.Names()
	if len(got) != len(want) {
		t.Fatalf("expected %d commands, got %d (%v)", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// Re-registration must fail (already registered).
	if err := registerBuiltinCommands(reg, nil); err == nil {
		t.Fatal("re-register expected to fail")
	}
}
