package policies

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hugr-lab/hugen/pkg/tool"
)

func TestPolicies_NilInner_DisabledSurface(t *testing.T) {
	p := New(nil, nil, nil)
	if p.IsConfigured() {
		t.Errorf("IsConfigured should be false on nil inner")
	}
	// callSave / callRevoke must surface ErrSystemUnavailable
	// rather than panicking on the nil inner.
	if _, err := p.Call(context.Background(), "save", json.RawMessage(`{}`)); !errors.Is(err, ErrSystemUnavailable) {
		t.Errorf("save on disabled = %v, want ErrSystemUnavailable", err)
	}
	if _, err := p.Call(context.Background(), "revoke", json.RawMessage(`{}`)); !errors.Is(err, ErrSystemUnavailable) {
		t.Errorf("revoke on disabled = %v, want ErrSystemUnavailable", err)
	}
}

func TestPolicies_List_NamesAndPermissions(t *testing.T) {
	p := New(nil, nil, nil)
	tools, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("List len = %d, want 2", len(tools))
	}
	want := map[string]string{
		"policy:save":   "hugen:policy:save",
		"policy:revoke": "hugen:policy:revoke",
	}
	for _, tl := range tools {
		got, ok := want[tl.Name]
		if !ok {
			t.Errorf("unexpected tool %q", tl.Name)
			continue
		}
		if tl.PermissionObject != got {
			t.Errorf("%s PermissionObject = %q, want %q", tl.Name, tl.PermissionObject, got)
		}
		if tl.Provider != "policy" {
			t.Errorf("%s Provider = %q, want policy", tl.Name, tl.Provider)
		}
		// Schemas must pass the LLM-schema gate (Anthropic / OpenAI / Gemini subset).
		if err := tool.ValidateLLMSchema(tl.ArgSchema); err != nil {
			t.Errorf("%s schema invalid: %v", tl.Name, err)
		}
	}
}

func TestPolicies_Call_UnknownToolReturnsError(t *testing.T) {
	p := New(nil, nil, nil)
	_, err := p.Call(context.Background(), "ghost", json.RawMessage(`{}`))
	if !errors.Is(err, tool.ErrUnknownTool) {
		t.Errorf("unknown tool err = %v, want ErrUnknownTool", err)
	}
}

func TestDecodeDecision_Aliases(t *testing.T) {
	cases := map[string]Outcome{
		"allow":           tool.PolicyAllow,
		"always_allowed":  tool.PolicyAllow,
		"deny":            tool.PolicyDeny,
		"denied":          tool.PolicyDeny,
		"ask":             tool.PolicyAsk,
		"manual_required": tool.PolicyAsk,
		"":                tool.PolicyAsk,
	}
	for in, want := range cases {
		got, err := decodeDecision(in)
		if err != nil {
			t.Errorf("decodeDecision(%q) err = %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("decodeDecision(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := decodeDecision("garbage"); err == nil {
		t.Errorf("decodeDecision(garbage) should error")
	}
}
