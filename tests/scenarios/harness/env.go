//go:build duckdb_arrow && scenario

package harness

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
)

// LoadDotEnv reads a .env-style file into a map. Missing files
// return (empty, nil) — the caller decides how to handle the
// absence (typically: "every run skips with a clear message").
//
// Format mirrors what cmd/hugen-test-token writes:
//
//   - lines starting with `#` are comments,
//   - blank lines are skipped,
//   - `KEY=value` pairs strip surrounding whitespace + double quotes
//     from the value,
//   - `${VAR}` references inside the value resolve against the
//     accumulating map first, then os.Environ. Loops would be
//     pathological — we don't recurse beyond one expansion pass.
func LoadDotEnv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	kv := make(map[string]string)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"`)
		kv[key] = expandVars(val, kv)
	}
	return kv, sc.Err()
}

// expandVars resolves ${VAR} references against `kv` first, then
// os.Environ. Single pass, no recursion — values that reference
// keys defined later in the file leak through verbatim, which is
// the same behaviour cmd/hugen and viper exhibit.
func expandVars(val string, kv map[string]string) string {
	return os.Expand(val, func(key string) string {
		if v, ok := kv[key]; ok {
			return v
		}
		return os.Getenv(key)
	})
}

// EvalRequires reports whether the harness should run a Run/
// Scenario whose `requires:` list mentions every key in `keys`.
// The mapping from a tag to the env vars that satisfy it lives
// here, not in the YAML, so individual scenario authors don't
// hard-code env-var names.
//
// Returns (true, "") when every required tag has a satisfied env
// var; (false, reason) when the first missing tag is reached.
func EvalRequires(env map[string]string, keys []string) (bool, string) {
	for _, k := range keys {
		needed, ok := requireMap[k]
		if !ok {
			return false, fmt.Sprintf("unknown require key %q", k)
		}
		for _, v := range needed {
			if env[v] == "" && os.Getenv(v) == "" {
				return false, fmt.Sprintf("require %q: %s not set", k, v)
			}
		}
	}
	return true, ""
}

// requireMap is the canonical tag → env-var list.
//
// "hugr" — needs an access token + endpoint; both modes (static
// HUGR_ACCESS_TOKEN/HUGR_TOKEN_URL or captured-via-OIDC) write
// HUGR_ACCESS_TOKEN, so checking that single key is enough.
//
// LLM keys are split per-provider so a run that asks for
// `[anthropic]` skips when the Anthropic key is missing even if
// other LLM keys are set.
var requireMap = map[string][]string{
	"hugr":      {"HUGR_URL", "HUGR_ACCESS_TOKEN"},
	"anthropic": {"ANTHROPIC_API_KEY"},
	"gemini":    {"GEMINI_API_KEY"},
	"openai":    {"OPENAI_API_KEY"},
	"local":     {"LLM_LOCAL_URL"},
}
