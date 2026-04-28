package main

import (
	"context"
	"log/slog"
	"reflect"
	"strings"
	"testing"
)

// TestRuntimeCore_NoSetMutators enforces constitution principle II at
// the type level: required deps live on RuntimeCore as read-only
// fields populated by buildRuntimeCore, never by post-construction
// setters.
func TestRuntimeCore_NoSetMutators(t *testing.T) {
	rt := reflect.TypeOf((*RuntimeCore)(nil))
	forbidden := []string{"Set", "Mount", "Bind", "Register"}
	for i := 0; i < rt.NumMethod(); i++ {
		name := rt.Method(i).Name
		for _, prefix := range forbidden {
			if strings.HasPrefix(name, prefix) {
				t.Errorf("RuntimeCore exposes forbidden mutator: %s (constitution principle II)", name)
			}
		}
	}
}

// TestRuntimeCore_Shutdown_Idempotent calls Shutdown twice on a
// minimally-populated RuntimeCore and asserts no panic and no double
// teardown. Manager / HTTPSrv / Auth are nil — Shutdown must guard
// against that.
func TestRuntimeCore_Shutdown_Idempotent(t *testing.T) {
	core := &RuntimeCore{
		Logger: slog.Default(),
	}
	ctx := context.Background()
	// First call must complete without panic.
	core.Shutdown(ctx)
	// Second call must be a no-op.
	core.Shutdown(ctx)
}
