package config

import (
	"testing"
	"time"
)

// TestLoadStaticInput_RecapBlock verifies the db-2 recap block decodes,
// including the time.Duration fold_timeout via the decode hook, and that
// it surfaces through the service view.
func TestLoadStaticInput_RecapBlock(t *testing.T) {
	in, err := LoadStaticInput(map[string]any{
		"recap": map[string]any{"fold_timeout": "30s", "max_message_tokens": 4096},
	}, true)
	if err != nil {
		t.Fatalf("LoadStaticInput: %v", err)
	}
	if in.Recap.FoldTimeout != 30*time.Second {
		t.Errorf("FoldTimeout = %v, want 30s", in.Recap.FoldTimeout)
	}
	if in.Recap.MaxMessageTokens != 4096 {
		t.Errorf("MaxMessageTokens = %d, want 4096", in.Recap.MaxMessageTokens)
	}
	svc := NewStaticService(in)
	if got := svc.Recap().FoldTimeout(); got != 30*time.Second {
		t.Errorf("Recap().FoldTimeout() = %v, want 30s", got)
	}
	if got := svc.Recap().MaxMessageTokens(); got != 4096 {
		t.Errorf("Recap().MaxMessageTokens() = %d, want 4096", got)
	}
	// Absent → zero (the extension then applies its own default).
	in2, _ := LoadStaticInput(map[string]any{}, true)
	if in2.Recap.FoldTimeout != 0 {
		t.Errorf("absent recap: FoldTimeout = %v, want 0", in2.Recap.FoldTimeout)
	}
}

func TestLoadStaticInput_ExpandsEnvAcrossAllSections(t *testing.T) {
	t.Setenv("HUGR_URL", "http://hugr.example.com:9000")
	t.Setenv("LLM_LOCAL_URL", "http://localhost:1234/v1")
	t.Setenv("STATE_DIR", "/var/lib/hugen")

	raw := map[string]any{
		"local_db": map[string]any{
			"db": map[string]any{
				"path": "${STATE_DIR}/engine.db",
				"settings": map[string]any{
					"home_directory": "${STATE_DIR}/data",
				},
			},
			"memory_path": "${STATE_DIR}/memory.db",
			"models": []any{
				map[string]any{
					"name": "gemma",
					"type": "llm-openai",
					"path": "${LLM_LOCAL_URL}?model=gemma",
				},
			},
		},
		"embedding": map[string]any{
			"mode":  "local",
			"model": "embed-${LLM_LOCAL_URL:0}", // placeholder reference still expands literal text
		},
		"models": map[string]any{
			"model": "gemma",
			"routes": map[string]any{
				"cheap": map[string]any{
					"model": "gemma-mini",
				},
			},
		},
		"auth": []any{
			map[string]any{
				"name":         "hugr",
				"type":         "hugr",
				"access_token": "${HUGR_ACCESS_TOKEN}",
				"token_url":    "${HUGR_URL}/auth/token",
			},
		},
		"tool_providers": []any{
			map[string]any{
				"name":      "hugr-main",
				"type":      "mcp",
				"transport": "http",
				"endpoint":  "${HUGR_URL}/mcp",
				"headers": map[string]any{
					"X-Trace": "${HUGR_URL}",
				},
				"env": map[string]any{
					"HUGR_IPC_URL": "${HUGR_URL}/ipc",
				},
				"args": []any{"--url=${HUGR_URL}"},
			},
		},
	}

	in, err := LoadStaticInput(raw, true)
	if err != nil {
		t.Fatalf("LoadStaticInput: %v", err)
	}

	if got := in.LocalDB.DB.Path; got != "/var/lib/hugen/engine.db" {
		t.Errorf("LocalDB.DB.Path not expanded: %q", got)
	}
	if got := in.LocalDB.DB.Settings.HomeDirectory; got != "/var/lib/hugen/data" {
		t.Errorf("LocalDB.DB.Settings.HomeDirectory: %q", got)
	}
	if got := in.LocalDB.MemoryPath; got != "/var/lib/hugen/memory.db" {
		t.Errorf("LocalDB.MemoryPath: %q", got)
	}
	if got := in.LocalDB.Models[0].Path; got != "http://localhost:1234/v1?model=gemma" {
		t.Errorf("LocalDB.Models[0].Path: %q", got)
	}
	if got := in.Auth[0].TokenURL; got != "http://hugr.example.com:9000/auth/token" {
		t.Errorf("Auth[0].TokenURL: %q", got)
	}
	if got := in.ToolProviders[0].Endpoint; got != "http://hugr.example.com:9000/mcp" {
		t.Errorf("ToolProviders[0].Endpoint: %q", got)
	}
	// Map keys must be preserved verbatim (env-vars are case-
	// sensitive; HTTP headers are conventionally Title-Case).
	if got := in.ToolProviders[0].Headers["X-Trace"]; got != "http://hugr.example.com:9000" {
		t.Errorf("ToolProviders[0].Headers[X-Trace]: %q", got)
	}
	if got := in.ToolProviders[0].Env["HUGR_IPC_URL"]; got != "http://hugr.example.com:9000/ipc" {
		t.Errorf("ToolProviders[0].Env[HUGR_IPC_URL]: %q", got)
	}
	if got := in.ToolProviders[0].Args[0]; got != "--url=http://hugr.example.com:9000" {
		t.Errorf("ToolProviders[0].Args[0]: %q", got)
	}
}

// TestLoadStaticInput_CompactorBlock verifies the phase-5.2 γ
// compactor schema decodes end-to-end through LoadStaticInput,
// including the per-tier overlay map + the tri-state Enabled
// pointer (absent vs. explicit zero).
func TestLoadStaticInput_CompactorBlock(t *testing.T) {
	raw := map[string]any{
		"compactor": map[string]any{
			"enabled":                true,
			"max_turns":              50,
			"max_tokens":             80000,
			"preserved_recent_turns": 10,
			"digest_max_tokens":      4000,
			"min_turn_gap":           3,
			"llm_timeout_ms":         30000,
			"llm_intent":             "summarize",
			"token_budget_ratio":     0.8,
			"tiers": map[string]any{
				"root": map[string]any{
					"enabled":                true,
					"preserved_recent_turns": 20,
					"token_budget_ratio":     0.7,
				},
				"mission": map[string]any{
					"enabled":                true,
					"preserved_recent_turns": 5,
					"max_turns":              40,
				},
				"worker": map[string]any{
					"enabled": false,
				},
			},
		},
	}
	in, err := LoadStaticInput(raw, true)
	if err != nil {
		t.Fatalf("LoadStaticInput: %v", err)
	}
	c := in.Compactor
	if c.Enabled == nil || !*c.Enabled {
		t.Errorf("top-level Enabled = %v, want &true", c.Enabled)
	}
	if c.MaxTurns != 50 {
		t.Errorf("MaxTurns = %d, want 50", c.MaxTurns)
	}
	if c.MaxTokens != 80000 {
		t.Errorf("MaxTokens = %d, want 80000", c.MaxTokens)
	}
	if c.TokenBudgetRatio != 0.8 {
		t.Errorf("TokenBudgetRatio = %v, want 0.8", c.TokenBudgetRatio)
	}
	if c.LLMIntent != "summarize" {
		t.Errorf("LLMIntent = %q, want summarize", c.LLMIntent)
	}
	if len(c.Tiers) != 3 {
		t.Fatalf("Tiers len = %d, want 3", len(c.Tiers))
	}

	root := c.Tiers["root"]
	if root.Enabled == nil || !*root.Enabled {
		t.Errorf("tiers.root.Enabled = %v, want &true", root.Enabled)
	}
	if root.PreservedRecentTurns == nil || *root.PreservedRecentTurns != 20 {
		t.Errorf("tiers.root.PreservedRecentTurns = %v, want &20", root.PreservedRecentTurns)
	}
	if root.TokenBudgetRatio == nil || *root.TokenBudgetRatio != 0.7 {
		t.Errorf("tiers.root.TokenBudgetRatio = %v, want &0.7", root.TokenBudgetRatio)
	}

	worker := c.Tiers["worker"]
	if worker.Enabled == nil || *worker.Enabled {
		t.Errorf("tiers.worker.Enabled = %v, want &false", worker.Enabled)
	}
	// Field that was absent in YAML should still be nil — the
	// tri-state pointer is the contract.
	if worker.MaxTurns != nil {
		t.Errorf("tiers.worker.MaxTurns = %v, want nil (absent)", worker.MaxTurns)
	}
}

// TestLoadStaticInput_SkillsInstallSet covers the install-set
// tri-state: absent key → nil (install all bundled), present list →
// authoritative names, present empty list → declared-but-empty
// (install nothing).
func TestLoadStaticInput_SkillsInstallSet(t *testing.T) {
	// Present with names → authoritative; pin parses alongside.
	in, err := LoadStaticInput(map[string]any{
		"skills": map[string]any{
			"install": []any{"analyst", "data_utils"},
			"pin":     []any{"analyst"},
		},
	}, true)
	if err != nil {
		t.Fatalf("LoadStaticInput: %v", err)
	}
	svc := NewStaticService(in)
	if !svc.Skills().InstallSetDeclared() {
		t.Errorf("present install list: InstallSetDeclared() = false, want true")
	}
	if got := svc.Skills().InstallSet(); len(got) != 2 || got[0] != "analyst" || got[1] != "data_utils" {
		t.Errorf("InstallSet() = %v, want [analyst data_utils]", got)
	}
	if !svc.Skills().PinSetDeclared() {
		t.Errorf("present pin list: PinSetDeclared() = false, want true")
	}
	if got := svc.Skills().PinSet(); len(got) != 1 || got[0] != "analyst" {
		t.Errorf("PinSet() = %v, want [analyst]", got)
	}

	// Present but empty → declared, installs nothing.
	in, err = LoadStaticInput(map[string]any{
		"skills": map[string]any{"install": []any{}},
	}, true)
	if err != nil {
		t.Fatalf("LoadStaticInput (empty): %v", err)
	}
	svc = NewStaticService(in)
	if !svc.Skills().InstallSetDeclared() {
		t.Errorf("empty install list: InstallSetDeclared() = false, want true")
	}
	if got := svc.Skills().InstallSet(); len(got) != 0 {
		t.Errorf("empty install list: InstallSet() = %v, want []", got)
	}

	// Absent key → nil (install all bundled); pin untouched.
	in, err = LoadStaticInput(map[string]any{}, true)
	if err != nil {
		t.Fatalf("LoadStaticInput (absent): %v", err)
	}
	svc = NewStaticService(in)
	if svc.Skills().InstallSetDeclared() {
		t.Errorf("absent skills block: InstallSetDeclared() = true, want false")
	}
	if got := svc.Skills().InstallSet(); got != nil {
		t.Errorf("absent skills block: InstallSet() = %v, want nil", got)
	}
	if svc.Skills().PinSetDeclared() {
		t.Errorf("absent skills block: PinSetDeclared() = true, want false")
	}
	if got := svc.Skills().PinSet(); got != nil {
		t.Errorf("absent skills block: PinSet() = %v, want nil", got)
	}
}

// TestLoadStaticInput_CompactorAbsentIsZeroValue verifies that
// a config without a `compactor:` key leaves the loaded
// CompactorConfig at its zero value — the runtime adapter
// applies the extension's DefaultConfig defaults on top.
func TestLoadStaticInput_CompactorAbsentIsZeroValue(t *testing.T) {
	in, err := LoadStaticInput(map[string]any{}, true)
	if err != nil {
		t.Fatalf("LoadStaticInput: %v", err)
	}
	if in.Compactor.Enabled != nil {
		t.Errorf("absent compactor block: Enabled = %v, want nil", in.Compactor.Enabled)
	}
	if in.Compactor.MaxTurns != 0 {
		t.Errorf("absent compactor block: MaxTurns = %d, want 0", in.Compactor.MaxTurns)
	}
	if len(in.Compactor.Tiers) != 0 {
		t.Errorf("absent compactor block: Tiers len = %d, want 0", len(in.Compactor.Tiers))
	}
}
