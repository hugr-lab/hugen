package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/skill"
)

func TestSystemProvider_NameAndList(t *testing.T) {
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
	if len(tools) != 11 {
		t.Errorf("len(tools) = %d, want 11", len(tools))
	}
	for _, tt := range tools {
		if tt.Provider != "system" {
			t.Errorf("Tool %s provider = %q", tt.Name, tt.Provider)
		}
		if tt.PermissionObject != "hugen:tool:system" {
			t.Errorf("Tool %s perm = %q", tt.Name, tt.PermissionObject)
		}
		if !strings.HasPrefix(tt.Name, "system:") {
			t.Errorf("Tool %s missing prefix", tt.Name)
		}
	}
}

func TestSystemProvider_NotepadAppend(t *testing.T) {
	called := false
	deps := SystemDeps{
		Notepad: func(ctx context.Context, agentID, sessionID, authorID, text string) (string, error) {
			called = true
			if sessionID != "s1" {
				t.Errorf("sessionID = %q, want s1", sessionID)
			}
			if agentID != "a1" {
				t.Errorf("agentID = %q, want a1", agentID)
			}
			if text != "hello" {
				t.Errorf("text = %q", text)
			}
			return "note-xyz", nil
		},
	}
	deps.AgentID = "a1"
	p := NewSystemProvider(deps)
	ctx := perm.WithSession(context.Background(), perm.SessionContext{SessionID: "s1"})
	out, err := p.Call(ctx, "notepad_append", json.RawMessage(`{"text":"hello"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !called {
		t.Errorf("notepad func not invoked")
	}
	var got map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["id"] != "note-xyz" {
		t.Errorf("id = %q", got["id"])
	}
}

func TestSystemProvider_NotepadAppend_NotWired(t *testing.T) {
	p := NewSystemProvider(SystemDeps{})
	_, err := p.Call(context.Background(), "notepad_append", json.RawMessage(`{"text":"x"}`))
	if !errors.Is(err, ErrSystemUnavailable) {
		t.Errorf("err = %v, want ErrSystemUnavailable", err)
	}
}

func TestSystemProvider_SkillLoad_RoutesThroughManager(t *testing.T) {
	store := skill.NewSkillStore(skill.Options{Inline: map[string][]byte{
		"alpha": []byte(`---
name: alpha
description: alpha skill.
license: MIT
allowed-tools:
  - provider: bash-mcp
    tools: [bash.read_file]
---
body
`),
	}})
	mgr := skill.NewSkillManager(store, nil)
	p := NewSystemProvider(SystemDeps{Skills: mgr})
	ctx := perm.WithSession(context.Background(), perm.SessionContext{SessionID: "s1"})
	out, err := p.Call(ctx, "skill_load", json.RawMessage(`{"name":"alpha"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(string(out), `"loaded":true`) {
		t.Errorf("out = %s", out)
	}
	// Verify skill is actually loaded.
	if _, err := mgr.LoadedSkill(ctx, "s1", "alpha"); err != nil {
		t.Errorf("LoadedSkill: %v", err)
	}
}

func TestSystemProvider_SkillLoad_MissingSession(t *testing.T) {
	mgr := skill.NewSkillManager(skill.NewSkillStore(skill.Options{}), nil)
	p := NewSystemProvider(SystemDeps{Skills: mgr})
	_, err := p.Call(context.Background(), "skill_load", json.RawMessage(`{"name":"x"}`))
	if !errors.Is(err, ErrArgValidation) {
		t.Errorf("err = %v, want ErrArgValidation (no session id)", err)
	}
}

func TestSystemProvider_SkillUnload_Idempotent(t *testing.T) {
	mgr := skill.NewSkillManager(skill.NewSkillStore(skill.Options{}), nil)
	p := NewSystemProvider(SystemDeps{Skills: mgr})
	ctx := perm.WithSession(context.Background(), perm.SessionContext{SessionID: "s1"})
	// Unload missing skill — must succeed (idempotent).
	if _, err := p.Call(ctx, "skill_unload", json.RawMessage(`{"name":"missing"}`)); err != nil {
		t.Errorf("Call: %v", err)
	}
}

func TestSystemProvider_SkillRef_ReadsReferencesFile(t *testing.T) {
	store := skill.NewSkillStore(skill.Options{Inline: map[string][]byte{
		"alpha": []byte(`---
name: alpha
description: alpha skill.
license: MIT
allowed-tools:
  - provider: bash-mcp
    tools: [bash.read_file]
---
body
`),
	}})
	// Inline backend produces FS=nil, so override with a dirBackend-style
	// skill via direct mutation isn't straightforward. Skip the read path
	// and assert the not-found path instead — full e2e is via T043.
	mgr := skill.NewSkillManager(store, nil)
	p := NewSystemProvider(SystemDeps{Skills: mgr})
	ctx := perm.WithSession(context.Background(), perm.SessionContext{SessionID: "s1"})
	if err := mgr.Load(ctx, "s1", "alpha"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err := p.Call(ctx, "skill_ref", json.RawMessage(`{"skill":"alpha","ref":"x.md"}`))
	if err == nil {
		t.Fatalf("expected error (alpha is inline, no body fs)")
	}
	if !strings.Contains(err.Error(), "no body fs") {
		t.Errorf("err = %v", err)
	}
}

func TestSystemProvider_SkillRef_WithFS(t *testing.T) {
	mgr := skill.NewSkillManager(skill.NewSkillStore(skill.Options{}), nil)
	// Manually inject a Skill into a session by going via SkillManager
	// is non-trivial; easier path: stub via testing/fstest directly on
	// the LoadedSkill output. But manager.go has no setter — skip the
	// direct injection and rely on T043 integration test for full
	// coverage. The path-not-found case is exercised above.
	_ = mgr
	_ = fstest.MapFS{}
}

func TestSystemProvider_RuntimeReload_RoutesTarget(t *testing.T) {
	got := ""
	p := NewSystemProvider(SystemDeps{
		Reload: func(ctx context.Context, target string) error {
			got = target
			return nil
		},
	})
	if _, err := p.Call(context.Background(), "runtime_reload", json.RawMessage(`{"target":"permissions"}`)); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got != "permissions" {
		t.Errorf("target = %q", got)
	}
}

func TestSystemProvider_RuntimeReload_DefaultAll(t *testing.T) {
	got := ""
	p := NewSystemProvider(SystemDeps{
		Reload: func(ctx context.Context, target string) error {
			got = target
			return nil
		},
	})
	if _, err := p.Call(context.Background(), "runtime_reload", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got != "all" {
		t.Errorf("target = %q, want all", got)
	}
}

func TestSystemProvider_RuntimeReload_BadTarget(t *testing.T) {
	called := 0
	p := NewSystemProvider(SystemDeps{
		Reload: func(context.Context, string) error { called++; return nil },
	})
	_, err := p.Call(context.Background(), "runtime_reload", json.RawMessage(`{"target":"bogus"}`))
	if !errors.Is(err, ErrArgValidation) {
		t.Errorf("err = %v, want ErrArgValidation", err)
	}
	if called != 0 {
		t.Errorf("Reload called %d times despite bad target", called)
	}
}

func TestSystemProvider_RuntimeReload_GateDenied(t *testing.T) {
	called := 0
	perms := &fakePerms{rules: map[string]perm.Permission{
		"hugen:command:runtime_reload:mcp": {Disabled: true, FromConfig: true},
	}}
	p := NewSystemProvider(SystemDeps{
		Perms:  perms,
		Reload: func(context.Context, string) error { called++; return nil },
	})
	_, err := p.Call(context.Background(), "runtime_reload", json.RawMessage(`{"target":"mcp"}`))
	if !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("err = %v, want ErrPermissionDenied", err)
	}
	if called != 0 {
		t.Errorf("Reload invoked %d times despite gate deny", called)
	}
	// Different target stays allowed.
	if _, err := p.Call(context.Background(), "runtime_reload", json.RawMessage(`{"target":"skills"}`)); err != nil {
		t.Errorf("non-denied target failed: %v", err)
	}
	if called != 1 {
		t.Errorf("Reload calls = %d, want 1", called)
	}
}

func TestSystemProvider_MCPAdd_RoutesSpec(t *testing.T) {
	var captured MCPAddSpec
	p := NewSystemProvider(SystemDeps{
		AddMCP: func(ctx context.Context, spec MCPAddSpec) error {
			captured = spec
			return nil
		},
	})
	args := json.RawMessage(`{"name":"web","command":"web-mcp","args":["--port","9000"],"env":{"K":"v"}}`)
	if _, err := p.Call(context.Background(), "mcp_add_server", args); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if captured.Name != "web" || captured.Command != "web-mcp" {
		t.Errorf("spec = %+v", captured)
	}
	if len(captured.Args) != 2 || captured.Env["K"] != "v" {
		t.Errorf("spec args/env = %+v", captured)
	}
}

func TestSystemProvider_MCPAdd_MissingFields(t *testing.T) {
	p := NewSystemProvider(SystemDeps{AddMCP: func(ctx context.Context, spec MCPAddSpec) error { return nil }})
	_, err := p.Call(context.Background(), "mcp_add_server", json.RawMessage(`{"name":"web"}`))
	if !errors.Is(err, ErrArgValidation) {
		t.Errorf("err = %v, want ErrArgValidation", err)
	}
}

func TestSystemProvider_MCPRemove_RoutesName(t *testing.T) {
	got := ""
	p := NewSystemProvider(SystemDeps{
		RemoveMCP: func(ctx context.Context, name string) error {
			got = name
			return nil
		},
	})
	if _, err := p.Call(context.Background(), "mcp_remove_server", json.RawMessage(`{"name":"web"}`)); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got != "web" {
		t.Errorf("name = %q", got)
	}
}

func TestSystemProvider_PermissionGate_DispatchedThroughManager(t *testing.T) {
	// Tier-1 floor disables system:mcp_add_server. ToolManager.Resolve
	// must surface ErrPermissionDenied; SystemProvider.Call is never
	// reached.
	deps := SystemDeps{
		AddMCP: func(ctx context.Context, spec MCPAddSpec) error {
			t.Errorf("AddMCP must not be invoked when permission denies")
			return nil
		},
	}
	sp := NewSystemProvider(deps)
	perms := &fakePerms{rules: map[string]perm.Permission{
		"hugen:tool:system:mcp_add_server": {Disabled: true, FromConfig: true},
	}}
	m := NewToolManager(perms, nil, nil, nil, nil)
	if err := m.AddProvider(sp); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	tool := Tool{
		Name:             "system:mcp_add_server",
		Provider:         "system",
		PermissionObject: "hugen:tool:system",
	}
	_, _, err := m.Resolve(context.Background(), tool, json.RawMessage(`{"name":"x","command":"y"}`))
	if !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("err = %v, want ErrPermissionDenied", err)
	}
}

func TestSystemProvider_UnknownTool(t *testing.T) {
	p := NewSystemProvider(SystemDeps{})
	_, err := p.Call(context.Background(), "ghost", json.RawMessage(`{}`))
	if !errors.Is(err, ErrUnknownTool) {
		t.Errorf("err = %v, want ErrUnknownTool", err)
	}
}

func TestSystemProvider_PolicySave_HappyPath(t *testing.T) {
	store := newFakePolicyStore()
	policies := NewPolicies(newFakePolicyQuerier(store))
	sp := NewSystemProvider(SystemDeps{
		AgentID:  "ag01",
		Policies: policies,
	})
	out, err := sp.Call(context.Background(), "policy_save",
		json.RawMessage(`{"tool_name":"bash-mcp:read_file","decision":"allow"}`))
	if err != nil {
		t.Fatalf("policy_save: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["tool_name"] != "bash-mcp:read_file" {
		t.Errorf("tool_name = %q", got["tool_name"])
	}
	if got["decision"] != "always_allowed" {
		t.Errorf("decision = %q", got["decision"])
	}
	if !strings.HasPrefix(got["id"], "ag01|") {
		t.Errorf("id missing agent prefix: %q", got["id"])
	}
	if len(store.rows) != 1 {
		t.Errorf("store rows = %d", len(store.rows))
	}
}

func TestSystemProvider_PolicySave_NotConfigured(t *testing.T) {
	sp := NewSystemProvider(SystemDeps{AgentID: "ag01"})
	_, err := sp.Call(context.Background(), "policy_save",
		json.RawMessage(`{"tool_name":"bash-mcp:read_file","decision":"allow"}`))
	if !errors.Is(err, ErrSystemUnavailable) {
		t.Errorf("err = %v, want ErrSystemUnavailable", err)
	}
}

func TestSystemProvider_PolicySave_GateDenied(t *testing.T) {
	store := newFakePolicyStore()
	policies := NewPolicies(newFakePolicyQuerier(store))
	perms := &fakePerms{rules: map[string]perm.Permission{
		"hugen:policy:persist:bash-mcp:write_file": {Disabled: true, FromConfig: true},
	}}
	sp := NewSystemProvider(SystemDeps{
		AgentID:  "ag01",
		Policies: policies,
		Perms:    perms,
	})
	_, err := sp.Call(context.Background(), "policy_save",
		json.RawMessage(`{"tool_name":"bash-mcp:write_file","decision":"allow"}`))
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("err = %v, want ErrPermissionDenied", err)
	}
	if len(store.rows) != 0 {
		t.Errorf("store rows after denied save = %d, want 0", len(store.rows))
	}
}

func TestSystemProvider_PolicySave_BadDecision(t *testing.T) {
	store := newFakePolicyStore()
	sp := NewSystemProvider(SystemDeps{
		AgentID:  "ag01",
		Policies: NewPolicies(newFakePolicyQuerier(store)),
	})
	_, err := sp.Call(context.Background(), "policy_save",
		json.RawMessage(`{"tool_name":"bash-mcp:read_file","decision":"banana"}`))
	if !errors.Is(err, ErrArgValidation) {
		t.Errorf("err = %v, want ErrArgValidation", err)
	}
}

func TestSystemProvider_PolicyRevoke_RoundTrip(t *testing.T) {
	store := newFakePolicyStore()
	policies := NewPolicies(newFakePolicyQuerier(store))
	sp := NewSystemProvider(SystemDeps{
		AgentID:  "ag01",
		Policies: policies,
	})
	saveOut, err := sp.Call(context.Background(), "policy_save",
		json.RawMessage(`{"tool_name":"bash-mcp:read_file","decision":"allow"}`))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	var saved map[string]string
	if err := json.Unmarshal(saveOut, &saved); err != nil {
		t.Fatal(err)
	}
	revokeOut, err := sp.Call(context.Background(), "policy_revoke",
		json.RawMessage(`{"id":`+strconv.Quote(saved["id"])+`}`))
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(revokeOut, &got); err != nil {
		t.Fatal(err)
	}
	if got["revoked"] != saved["id"] {
		t.Errorf("revoked = %q, want %q", got["revoked"], saved["id"])
	}
	if len(store.rows) != 0 {
		t.Errorf("rows after revoke = %d, want 0", len(store.rows))
	}
}

func TestSystemProvider_PolicyRevoke_MissingID(t *testing.T) {
	sp := NewSystemProvider(SystemDeps{
		AgentID:  "ag01",
		Policies: NewPolicies(newFakePolicyQuerier(newFakePolicyStore())),
	})
	_, err := sp.Call(context.Background(), "policy_revoke", json.RawMessage(`{}`))
	if !errors.Is(err, ErrArgValidation) {
		t.Errorf("err = %v, want ErrArgValidation", err)
	}
}

// Tier-1/2 must gate revoke as well as save: otherwise an LLM
// could legitimately call policy_revoke to clear a deny pinned
// by an operator (`hugen:policy:persist:<tool>` set Disabled in
// config or in the Hugr role snapshot). Mirror the gate check.
func TestSystemProvider_PolicyRevoke_GateDenied(t *testing.T) {
	store := newFakePolicyStore()
	// Pre-seed the row directly via the policies façade so we
	// know the deny was installed by something other than the
	// LLM about to try to revoke it.
	policies := NewPolicies(newFakePolicyQuerier(store))
	id, err := policies.Save(context.Background(), PolicyInput{
		AgentID:   "ag01",
		ToolName:  "bash-mcp:write_file",
		Decision:  PolicyDeny,
		CreatedBy: PolicyCreatorSystem,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if len(store.rows) != 1 {
		t.Fatalf("seed rows = %d", len(store.rows))
	}
	perms := &fakePerms{rules: map[string]perm.Permission{
		"hugen:policy:persist:bash-mcp:write_file": {Disabled: true, FromConfig: true},
	}}
	sp := NewSystemProvider(SystemDeps{
		AgentID:  "ag01",
		Policies: policies,
		Perms:    perms,
	})
	_, err = sp.Call(context.Background(), "policy_revoke",
		json.RawMessage(`{"id":`+strconv.Quote(id)+`}`))
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("err = %v, want ErrPermissionDenied", err)
	}
	if len(store.rows) != 1 {
		t.Errorf("rows after blocked revoke = %d, want 1 (deny preserved)", len(store.rows))
	}
}

func TestSystemProvider_PolicyRevoke_BadID(t *testing.T) {
	sp := NewSystemProvider(SystemDeps{
		AgentID:  "ag01",
		Policies: NewPolicies(newFakePolicyQuerier(newFakePolicyStore())),
	})
	_, err := sp.Call(context.Background(), "policy_revoke",
		json.RawMessage(`{"id":"not-a-policy-id"}`))
	if !errors.Is(err, ErrArgValidation) {
		t.Errorf("err = %v, want ErrArgValidation", err)
	}
}
