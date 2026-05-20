package mission

import (
	"strings"
	"testing"
	"time"
)

func TestResolveDependsOn_EmptyAndNil(t *testing.T) {
	store := NewHandoffs()
	out, err := ResolveDependsOn(nil, store)
	if err != nil {
		t.Fatalf("nil deps: unexpected error %v", err)
	}
	if out != "" {
		t.Fatalf("nil deps: got %q, want empty", out)
	}

	if _, err := ResolveDependsOn([]string{"x@w"}, nil); err == nil {
		t.Fatal("nil store: want error, got nil")
	}
}

func TestResolveDependsOn_ResolvedBody(t *testing.T) {
	store := NewHandoffs()
	store.Put(Handoff{
		Ref:    "schema-orders@schema-discovery",
		Kind:   KindHandoff,
		Status: "ok",
		Body:   "table orders has columns: id, customer_id, total",
		Subagent: SubagentRef{
			Name: "schema-orders",
			Role: "schema-explorer",
		},
		CreatedAt: time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC),
	})

	out, err := ResolveDependsOn([]string{"schema-orders@schema-discovery"}, store)
	if err != nil {
		t.Fatalf("resolve: unexpected error %v", err)
	}
	if !strings.Contains(out, "[Resolved depends_on]") {
		t.Errorf("output missing header:\n%s", out)
	}
	if !strings.Contains(out, "<schema-orders@schema-discovery (role: schema-explorer, status: ok)>") {
		t.Errorf("output missing labeled ref line:\n%s", out)
	}
	if !strings.Contains(out, "table orders has columns") {
		t.Errorf("output missing body:\n%s", out)
	}
}

func TestResolveDependsOn_MissingRefIsError(t *testing.T) {
	store := NewHandoffs()
	if _, err := ResolveDependsOn([]string{"missing@w"}, store); err == nil {
		t.Fatal("want error for missing ref, got nil")
	}
}

func TestRenderHandoffCatalog(t *testing.T) {
	store := NewHandoffs()
	if out := RenderHandoffCatalog(store); out != "" {
		t.Fatalf("empty store catalog = %q, want empty", out)
	}
	if out := RenderHandoffCatalog(nil); out != "" {
		t.Fatalf("nil store catalog = %q, want empty", out)
	}

	store.Put(Handoff{
		Ref:    "a@w1",
		Status: "ok",
		Subagent: SubagentRef{
			Role: "schema-explorer",
		},
		CreatedAt: time.Now(),
	})
	store.Put(Handoff{
		Ref:    "b@w1",
		Status: "error",
		Reason: "no data",
		Subagent: SubagentRef{
			Role: "query-builder",
		},
		CreatedAt: time.Now().Add(time.Second),
	})

	out := RenderHandoffCatalog(store)
	if !strings.Contains(out, "[Available handoffs]") {
		t.Errorf("catalog missing header:\n%s", out)
	}
	if !strings.Contains(out, "a@w1") || !strings.Contains(out, "b@w1") {
		t.Errorf("catalog missing refs:\n%s", out)
	}
	if !strings.Contains(out, "schema-explorer, ok") {
		t.Errorf("catalog missing role+status for a@w1:\n%s", out)
	}
	if !strings.Contains(out, "query-builder, error") {
		t.Errorf("catalog missing role+status for b@w1:\n%s", out)
	}
}
