//go:build duckdb_arrow && scenario

package harness

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnv_MissingFile(t *testing.T) {
	kv, err := LoadDotEnv(filepath.Join(t.TempDir(), "absent.env"))
	if err != nil {
		t.Fatalf("LoadDotEnv: %v", err)
	}
	if len(kv) != 0 {
		t.Errorf("missing file should yield empty map, got %v", kv)
	}
}

func TestLoadDotEnv_BasicParse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	body := "# comment\n" +
		"FOO=plain\n" +
		"BAR=\"quoted value\"\n" +
		"\n" +
		"REF=${FOO}-and-${OTHER}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("OTHER", "from-env")

	kv, err := LoadDotEnv(path)
	if err != nil {
		t.Fatalf("LoadDotEnv: %v", err)
	}
	if kv["FOO"] != "plain" {
		t.Errorf("FOO = %q", kv["FOO"])
	}
	if kv["BAR"] != "quoted value" {
		t.Errorf("BAR = %q", kv["BAR"])
	}
	if kv["REF"] != "plain-and-from-env" {
		t.Errorf("REF = %q (expected ${VAR} expansion against kv first, env second)", kv["REF"])
	}
}

func TestEvalRequires(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("HUGR_URL", "")
	t.Setenv("HUGR_ACCESS_TOKEN", "")

	tcs := []struct {
		name   string
		env    map[string]string
		keys   []string
		wantOK bool
	}{
		{"empty requires", map[string]string{}, nil, true},
		{"hugr satisfied", map[string]string{
			"HUGR_URL":          "http://h",
			"HUGR_ACCESS_TOKEN": "tok",
		}, []string{"hugr"}, true},
		{"hugr missing token", map[string]string{
			"HUGR_URL": "http://h",
		}, []string{"hugr"}, false},
		{"unknown key", map[string]string{}, []string{"mystery"}, false},
		{"anthropic via env", map[string]string{"ANTHROPIC_API_KEY": "k"}, []string{"anthropic"}, true},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := EvalRequires(tc.env, tc.keys)
			if ok != tc.wantOK {
				t.Errorf("EvalRequires = (%v, %q), want ok=%v", ok, reason, tc.wantOK)
			}
		})
	}
}
