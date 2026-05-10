package skill

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/fstest"
)

func newTestManager(t *testing.T, inline map[string][]byte) *SkillManager {
	t.Helper()
	store := NewSkillStore(Options{Inline: inline})
	return NewSkillManager(store, nil)
}

// fakeSink records OnSkillRefreshed invocations so tests can
// assert manager-side broadcast behaviour without a live skill
// extension.
type fakeSink struct {
	id  string
	mu  sync.Mutex
	got []Skill
}

func (s *fakeSink) SessionID() string { return s.id }
func (s *fakeSink) OnSkillRefreshed(sk Skill) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, sk)
}
func (s *fakeSink) refreshed() []Skill {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]Skill(nil), s.got...)
	return out
}

func TestResolveClosure_Single(t *testing.T) {
	m := newTestManager(t, map[string][]byte{
		"alpha": []byte(`---
name: alpha
description: alpha skill.
license: MIT
allowed-tools:
  - provider: bash-mcp
    tools: [bash.read_file]
---
Body.
`),
	})
	closure, err := m.ResolveClosure(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("ResolveClosure: %v", err)
	}
	if len(closure) != 1 || closure[0].Manifest.Name != "alpha" {
		t.Errorf("closure = %+v", closure)
	}
}

func TestResolveClosure_Transitive(t *testing.T) {
	m := newTestManager(t, map[string][]byte{
		"alpha": []byte(`---
name: alpha
description: alpha skill.
license: MIT
metadata:
  hugen:
    requires: [beta]
allowed-tools:
  - provider: bash-mcp
    tools: [bash.read_file]
---
`),
		"beta": []byte(`---
name: beta
description: beta skill.
license: MIT
metadata:
  hugen:
    requires: [gamma]
allowed-tools:
  - provider: bash-mcp
    tools: [bash.write_file]
---
`),
		"gamma": []byte(`---
name: gamma
description: leaf skill.
license: MIT
allowed-tools:
  - provider: bash-mcp
    tools: [bash.list_dir]
---
`),
	})
	closure, err := m.ResolveClosure(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("ResolveClosure: %v", err)
	}
	// Dependencies precede dependents: gamma, beta, alpha.
	if len(closure) != 3 {
		t.Fatalf("closure size = %d, want 3", len(closure))
	}
	want := []string{"gamma", "beta", "alpha"}
	for i, w := range want {
		if closure[i].Manifest.Name != w {
			t.Errorf("closure[%d] = %q, want %q", i, closure[i].Manifest.Name, w)
		}
	}
}

func TestResolveClosure_CycleRejected(t *testing.T) {
	m := newTestManager(t, map[string][]byte{
		"alpha": []byte(`---
name: alpha
description: cycles back via beta.
license: MIT
metadata:
  hugen:
    requires: [beta]
allowed-tools:
  - provider: bash-mcp
    tools: [bash.read_file]
---
`),
		"beta": []byte(`---
name: beta
description: cycles back via alpha.
license: MIT
metadata:
  hugen:
    requires: [alpha]
allowed-tools:
  - provider: bash-mcp
    tools: [bash.write_file]
---
`),
	})
	_, err := m.ResolveClosure(context.Background(), "alpha")
	if !errors.Is(err, ErrSkillCycle) {
		t.Errorf("expected ErrSkillCycle, got %v", err)
	}
}

func TestResolveClosure_MissingSkill(t *testing.T) {
	m := newTestManager(t, map[string][]byte{})
	_, err := m.ResolveClosure(context.Background(), "missing")
	if !errors.Is(err, ErrSkillNotFound) {
		t.Errorf("expected ErrSkillNotFound, got %v", err)
	}
}

func TestRegisterSink_BroadcastOnRefresh(t *testing.T) {
	m := newTestManager(t, map[string][]byte{
		"alpha": []byte(`---
name: alpha
description: alpha v1.
license: MIT
allowed-tools:
  - provider: bash-mcp
    tools: [bash.read_file]
---
v1
`),
	})
	sink := &fakeSink{id: "ses-1"}
	m.RegisterSink(sink)

	if _, err := m.Refresh(context.Background(), "alpha"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	got := sink.refreshed()
	if len(got) != 1 || got[0].Manifest.Name != "alpha" {
		t.Errorf("refresh broadcast = %+v", got)
	}
}

func TestDeregisterSink_StopsBroadcast(t *testing.T) {
	m := newTestManager(t, map[string][]byte{
		"alpha": []byte(`---
name: alpha
description: alpha.
license: MIT
allowed-tools:
  - provider: bash-mcp
    tools: [bash.read_file]
---
`),
	})
	sink := &fakeSink{id: "ses-deregister"}
	m.RegisterSink(sink)
	m.DeregisterSink(sink.id)
	if _, err := m.Refresh(context.Background(), "alpha"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := sink.refreshed(); len(got) != 0 {
		t.Errorf("expected no broadcast after deregister, got %+v", got)
	}
}

func TestRegisterSink_Idempotent(t *testing.T) {
	m := newTestManager(t, map[string][]byte{
		"alpha": []byte(`---
name: alpha
description: alpha.
license: MIT
allowed-tools:
  - provider: bash-mcp
    tools: [bash.read_file]
---
`),
	})
	sink := &fakeSink{id: "ses-dup"}
	m.RegisterSink(sink)
	m.RegisterSink(sink) // second call replaces in place — no double broadcast
	if _, err := m.Refresh(context.Background(), "alpha"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := sink.refreshed(); len(got) != 1 {
		t.Errorf("expected single broadcast for idempotent register, got %d", len(got))
	}
}

func TestSubscribe_DeliversRefreshAndPublish(t *testing.T) {
	m := newTestManager(t, map[string][]byte{
		"alpha": []byte(`---
name: alpha
description: alpha.
license: MIT
allowed-tools:
  - provider: bash-mcp
    tools: [bash.read_file]
---
`),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := m.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if _, err := m.Refresh(context.Background(), "alpha"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	select {
	case ev := <-ch:
		if ev.Kind != SkillRefreshed {
			t.Errorf("kind = %v, want refreshed", ev.Kind)
		}
	default:
		t.Errorf("no event delivered after Refresh")
	}
}

func TestPublishEmitsEvent(t *testing.T) {
	store := NewSkillStore(Options{
		LocalRoot: t.TempDir(),
	})
	m := NewSkillManager(store, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := m.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	manifest := Manifest{
		Name:        "published-skill",
		Description: "test",
		License:     "MIT",
		AllowedTools: AllowedTools{
			{Provider: "bash-mcp", Tools: []string{"bash.read_file"}},
		},
	}
	body := fstest.MapFS{}
	if err := m.Publish(ctx, manifest, body, PublishOptions{}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case ev := <-ch:
		if ev.Kind != SkillPublished {
			t.Errorf("kind = %v, want published", ev.Kind)
		}
		if ev.SkillName != "published-skill" {
			t.Errorf("name = %q", ev.SkillName)
		}
	default:
		t.Errorf("no event after Publish")
	}
}

func TestRefreshBumpsGeneration(t *testing.T) {
	m := newTestManager(t, map[string][]byte{
		"alpha": []byte(`---
name: alpha
description: alpha.
license: MIT
allowed-tools:
  - provider: bash-mcp
    tools: [bash.read_file]
---
`),
	})
	g0 := m.Gen()
	if _, err := m.Refresh(context.Background(), "alpha"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if m.Gen() <= g0 {
		t.Errorf("Gen did not bump: %d → %d", g0, m.Gen())
	}
}
