package skill

import (
	"context"
	"errors"
	"testing"
	"testing/fstest"
)

func newTestManager(t *testing.T, inline map[string][]byte) *SkillManager {
	t.Helper()
	store := NewSkillStore(Options{Inline: inline})
	return NewSkillManager(store, nil)
}

func TestManager_LoadAndBindings(t *testing.T) {
	m := newTestManager(t, map[string][]byte{
		"alpha": []byte(`---
name: alpha
description: alpha skill.
license: MIT
allowed-tools:
  - provider: bash-mcp
    tools: [bash.read_file]
---
Body of alpha.
`),
	})

	ctx := context.Background()
	if err := m.Load(ctx, "sess-1", "alpha"); err != nil {
		t.Fatalf("Load error: %v", err)
	}

	b, err := m.Bindings(ctx, "sess-1")
	if err != nil {
		t.Fatalf("Bindings error: %v", err)
	}
	if b.Generation == 0 {
		t.Errorf("Bindings.Generation = 0, want non-zero")
	}
	if len(b.AllowedTools) != 1 || b.AllowedTools[0].Provider != "bash-mcp" {
		t.Errorf("AllowedTools = %+v", b.AllowedTools)
	}
	if b.Instructions == "" {
		t.Errorf("Instructions empty, want body content")
	}
}

func TestManager_LoadCycleRejected(t *testing.T) {
	m := newTestManager(t, map[string][]byte{
		"a": []byte(`---
name: a
description: a depends on b.
license: MIT
metadata:
  hugen:
    requires: [b]
---
`),
		"b": []byte(`---
name: b
description: b depends on a.
license: MIT
metadata:
  hugen:
    requires: [a]
---
`),
	})

	err := m.Load(context.Background(), "s", "a")
	if err == nil {
		t.Fatal("Load(cycle) returned nil error")
	}
	if !errors.Is(err, ErrSkillCycle) {
		t.Errorf("err = %v, want ErrSkillCycle", err)
	}
}

func TestManager_LoadResolvesTransitiveDeps(t *testing.T) {
	m := newTestManager(t, map[string][]byte{
		"top": []byte(`---
name: top
description: top
license: MIT
metadata:
  hugen:
    requires: [mid]
---
`),
		"mid": []byte(`---
name: mid
description: mid
license: MIT
metadata:
  hugen:
    requires: [base]
---
`),
		"base": []byte(`---
name: base
description: base
license: MIT
allowed-tools:
  - provider: bash-mcp
    tools: [bash.read_file]
---
`),
	})

	if err := m.Load(context.Background(), "s", "top"); err != nil {
		t.Fatalf("Load error: %v", err)
	}

	b, err := m.Bindings(context.Background(), "s")
	if err != nil {
		t.Fatalf("Bindings error: %v", err)
	}
	// Base contributed allowed-tools through transitive load.
	if len(b.AllowedTools) != 1 {
		t.Errorf("AllowedTools = %+v, want one entry from base", b.AllowedTools)
	}
}

// TestManager_LoadResolvesRequiresSkills_PhaseFour mirrors the
// transitive-deps coverage above using the phase-4 canonical
// `requires_skills` key — proves the closure resolver consumes the
// new key alongside the legacy `requires`.
func TestManager_LoadResolvesRequiresSkills_PhaseFour(t *testing.T) {
	m := newTestManager(t, map[string][]byte{
		"top": []byte(`---
name: top
description: top
license: MIT
metadata:
  hugen:
    requires_skills: [base]
---
`),
		"base": []byte(`---
name: base
description: base
license: MIT
allowed-tools:
  - provider: bash-mcp
    tools: [bash.read_file]
---
`),
	})

	if err := m.Load(context.Background(), "s", "top"); err != nil {
		t.Fatalf("Load error: %v", err)
	}
	b, err := m.Bindings(context.Background(), "s")
	if err != nil {
		t.Fatalf("Bindings: %v", err)
	}
	if len(b.AllowedTools) != 1 {
		t.Errorf("AllowedTools = %+v, want one entry from base via requires_skills", b.AllowedTools)
	}
}

func TestManager_LoadMissingSkill(t *testing.T) {
	m := newTestManager(t, nil)
	err := m.Load(context.Background(), "s", "ghost")
	if err == nil {
		t.Fatal("Load(missing) returned nil error")
	}
	if !errors.Is(err, ErrSkillNotFound) {
		t.Errorf("err = %v, want to wrap ErrSkillNotFound", err)
	}
}

func TestManager_UnloadIdempotent(t *testing.T) {
	m := newTestManager(t, map[string][]byte{
		"x": []byte(`---
name: x
description: x
license: MIT
---
`),
	})
	ctx := context.Background()
	if err := m.Load(ctx, "s", "x"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := m.Unload(ctx, "s", "x"); err != nil {
		t.Fatalf("Unload: %v", err)
	}
	// Second Unload is a no-op, no error.
	if err := m.Unload(ctx, "s", "x"); err != nil {
		t.Errorf("Unload (twice): %v", err)
	}
	// Unloading from a session that doesn't exist is a no-op.
	if err := m.Unload(ctx, "no-such-session", "x"); err != nil {
		t.Errorf("Unload (unknown session): %v", err)
	}
}

func TestManager_Bindings_GenerationStableWithinTurn(t *testing.T) {
	m := newTestManager(t, map[string][]byte{
		"x": []byte(`---
name: x
description: x
license: MIT
---
`),
	})
	ctx := context.Background()
	if err := m.Load(ctx, "s", "x"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	b1, _ := m.Bindings(ctx, "s")
	b2, _ := m.Bindings(ctx, "s")
	if b1.Generation != b2.Generation {
		t.Errorf("Generation moved without a Load/Unload: %d vs %d", b1.Generation, b2.Generation)
	}
}

func TestManager_Bindings_GenerationMovesOnLoad(t *testing.T) {
	m := newTestManager(t, map[string][]byte{
		"a": []byte(`---
name: a
description: a
license: MIT
---
`),
		"b": []byte(`---
name: b
description: b
license: MIT
---
`),
	})
	ctx := context.Background()
	_ = m.Load(ctx, "s", "a")
	b1, _ := m.Bindings(ctx, "s")
	_ = m.Load(ctx, "s", "b")
	b2, _ := m.Bindings(ctx, "s")
	if b2.Generation <= b1.Generation {
		t.Errorf("Generation did not move on second Load: %d -> %d", b1.Generation, b2.Generation)
	}
}

func TestManager_Subscribe_DeliversLoadedAndUnloaded(t *testing.T) {
	m := newTestManager(t, map[string][]byte{
		"x": []byte(`---
name: x
description: x
license: MIT
---
`),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := m.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe error: %v", err)
	}

	if err := m.Load(ctx, "s", "x"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := m.Unload(ctx, "s", "x"); err != nil {
		t.Fatalf("Unload: %v", err)
	}

	gotKinds := []SkillChangeKind{}
	for i := 0; i < 2; i++ {
		select {
		case ev := <-ch:
			gotKinds = append(gotKinds, ev.Kind)
		default:
			t.Fatalf("expected event %d, got nothing", i+1)
		}
	}
	if gotKinds[0] != SkillLoaded || gotKinds[1] != SkillUnloaded {
		t.Errorf("event order = %v, want [Loaded, Unloaded]", gotKinds)
	}
}

func TestManager_PublishEmitsEvent(t *testing.T) {
	store := NewSkillStore(Options{LocalRoot: t.TempDir()})
	m := NewSkillManager(store, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, _ := m.Subscribe(ctx)

	manifest, _ := Parse([]byte(`---
name: pub
description: published
license: MIT
---
`))

	if err := m.Publish(ctx, manifest, fstest.MapFS{}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case ev := <-ch:
		if ev.Kind != SkillPublished {
			t.Errorf("event kind = %v, want SkillPublished", ev.Kind)
		}
		if ev.SkillName != "pub" {
			t.Errorf("event skill = %q, want pub", ev.SkillName)
		}
	default:
		t.Fatal("Publish event not delivered")
	}
}

func TestManager_RefreshBumpsGeneration(t *testing.T) {
	m := newTestManager(t, map[string][]byte{
		"x": []byte(`---
name: x
description: x
license: MIT
---
`),
	})
	ctx := context.Background()
	_ = m.Load(ctx, "s", "x")
	b1, _ := m.Bindings(ctx, "s")
	gen, err := m.Refresh(ctx, "x")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	b2, _ := m.Bindings(ctx, "s")
	if b2.Generation != gen {
		t.Errorf("Bindings.Generation = %d, want %d (Refresh return)", b2.Generation, gen)
	}
	if b2.Generation <= b1.Generation {
		t.Errorf("Refresh did not move generation: %d -> %d", b1.Generation, b2.Generation)
	}
}
