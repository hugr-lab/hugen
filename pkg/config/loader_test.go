package config

import (
	"testing"
)

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
