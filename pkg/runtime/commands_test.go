package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

func TestCommandRegistry_Register(t *testing.T) {
	r := NewCommandRegistry()
	noop := func(_ context.Context, _ CommandEnv, _ []string) ([]protocol.Frame, error) {
		return nil, nil
	}
	if err := r.Register("ping", CommandSpec{Handler: noop, Description: "test"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.Register("ping", CommandSpec{Handler: noop, Description: "dup"}); !errors.Is(err, ErrCommandExists) {
		t.Fatalf("expected ErrCommandExists, got %v", err)
	}
	if err := r.Register("Bad", CommandSpec{Handler: noop, Description: "x"}); !errors.Is(err, ErrCommandInvalidName) {
		t.Fatalf("expected ErrCommandInvalidName for uppercase, got %v", err)
	}
	if err := r.Register("9foo", CommandSpec{Handler: noop, Description: "x"}); !errors.Is(err, ErrCommandInvalidName) {
		t.Fatalf("expected ErrCommandInvalidName for leading digit, got %v", err)
	}
	if _, ok := r.Lookup("ping"); !ok {
		t.Fatal("Lookup(ping) miss")
	}
	if _, ok := r.Lookup("nope"); ok {
		t.Fatal("Lookup(nope) hit unexpectedly")
	}
}

func TestCommandRegistry_Describe(t *testing.T) {
	r := NewCommandRegistry()
	noop := func(_ context.Context, _ CommandEnv, _ []string) ([]protocol.Frame, error) {
		return nil, nil
	}
	_ = r.Register("foo", CommandSpec{Handler: noop, Description: "first"})
	_ = r.Register("bar", CommandSpec{Handler: noop, Description: "second"})
	out := r.Describe()
	if out == "" {
		t.Fatal("describe empty")
	}
	if got := r.Names(); len(got) != 2 || got[0] != "bar" || got[1] != "foo" {
		t.Errorf("Names = %v", got)
	}
}
