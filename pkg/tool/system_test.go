package tool

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// SystemProvider is a vestige of phase 4.1a steps 20-25 — every
// tool migrated to a dedicated provider. The skeleton stays alive
// until step 27 deletes the type. These tests assert the empty
// invariants so a future regression that re-attaches a tool here
// without going through the migration trips a CI failure.

func TestSystemProvider_EmptyCatalog(t *testing.T) {
	p := NewSystemProvider(SystemDeps{})
	if p.Name() != "system" {
		t.Errorf("Name = %q", p.Name())
	}
	if p.Lifetime() != LifetimePerAgent {
		t.Errorf("Lifetime = %v", p.Lifetime())
	}
	tools, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tools) != 0 {
		t.Errorf("len(tools) = %d, want 0 (SystemProvider is empty post-step-25)", len(tools))
	}
}

func TestSystemProvider_UnknownTool(t *testing.T) {
	p := NewSystemProvider(SystemDeps{})
	_, err := p.Call(context.Background(), "ghost", json.RawMessage(`{}`))
	if !errors.Is(err, ErrUnknownTool) {
		t.Errorf("err = %v, want ErrUnknownTool", err)
	}
}
