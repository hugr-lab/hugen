package spawn

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

type fakeSource struct {
	name string
	env  map[string]string
}

func (f fakeSource) Name() string { return f.name }
func (f fakeSource) Env(_ context.Context, _ string) (map[string]string, func(), error) {
	return f.env, func() {}, nil
}

func TestSources_RegisterAndGet(t *testing.T) {
	reg := NewSources()
	if err := reg.Register(fakeSource{name: "hugr"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	src, ok := reg.Get("hugr")
	if !ok {
		t.Fatalf("hugr not found after register")
	}
	if src.Name() != "hugr" {
		t.Fatalf("Name = %q want hugr", src.Name())
	}
	if _, ok := reg.Get("missing"); ok {
		t.Fatalf("missing should not be present")
	}
}

func TestSources_RegisterDuplicate(t *testing.T) {
	reg := NewSources()
	_ = reg.Register(fakeSource{name: "hugr"})
	err := reg.Register(fakeSource{name: "hugr"})
	if err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("dup register err = %v", err)
	}
}

func TestSources_RegisterEmpty(t *testing.T) {
	reg := NewSources()
	if err := reg.Register(fakeSource{name: ""}); err == nil {
		t.Fatalf("empty name should error")
	}
	if err := reg.Register(nil); err == nil {
		t.Fatalf("nil source should error")
	}
}

func TestSources_NamesSorted(t *testing.T) {
	reg := NewSources()
	_ = reg.Register(fakeSource{name: "z"})
	_ = reg.Register(fakeSource{name: "a"})
	_ = reg.Register(fakeSource{name: "m"})
	got := reg.Names()
	want := []string{"a", "m", "z"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Names = %v want %v", got, want)
	}
}
