package skill

import (
	"errors"
	"strings"
	"testing"
)

func TestParse_HappyPath(t *testing.T) {
	src := `---
name: hello-world
description: A trivial skill that says hi.
license: MIT
compatibility:
  model: claude-sonnet-4
  runtime: hugen>=0.3.0
allowed-tools:
  - provider: bash-mcp
    tools:
      - bash.read_file
      - bash.write_file
metadata:
  hugen:
    requires: [_memory]
    intents: [demo]
---
# hello-world

Body here.
`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if m.Name != "hello-world" {
		t.Errorf("Name = %q, want hello-world", m.Name)
	}
	if m.License != "MIT" {
		t.Errorf("License = %q, want MIT", m.License)
	}
	if m.Compatibility.Model != "claude-sonnet-4" {
		t.Errorf("Compatibility.Model = %q", m.Compatibility.Model)
	}
	if len(m.AllowedTools) != 1 || m.AllowedTools[0].Provider != "bash-mcp" {
		t.Errorf("AllowedTools = %+v", m.AllowedTools)
	}
	if got := m.AllowedTools[0].Tools; len(got) != 2 {
		t.Errorf("AllowedTools[0].Tools len = %d, want 2", len(got))
	}
	if !contains(m.Hugen.Requires, "_memory") {
		t.Errorf("Hugen.Requires = %v, want to contain _memory", m.Hugen.Requires)
	}
	if !contains(m.Hugen.Intents, "demo") {
		t.Errorf("Hugen.Intents = %v, want to contain demo", m.Hugen.Intents)
	}
	if !strings.Contains(string(m.Body), "Body here") {
		t.Errorf("Body did not retain markdown content: %q", string(m.Body))
	}
}

func TestParse_FrontmatterOnly(t *testing.T) {
	src := `---
name: empty-body
description: Frontmatter only.
license: MIT
---
`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if m.Name != "empty-body" {
		t.Errorf("Name = %q", m.Name)
	}
	if got := strings.TrimSpace(string(m.Body)); got != "" {
		t.Errorf("Body = %q, want empty", got)
	}
}

func TestParse_NoFrontmatterRejected(t *testing.T) {
	src := `# A markdown file without frontmatter`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("Parse(no-frontmatter) returned nil error")
	}
	if !errors.Is(err, ErrManifestInvalid) {
		t.Errorf("err = %v, want ErrManifestInvalid", err)
	}
}

func TestParse_UnclosedFrontmatterRejected(t *testing.T) {
	src := `---
name: dangling
description: oops
`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("Parse(unclosed) returned nil error")
	}
	if !errors.Is(err, ErrManifestInvalid) {
		t.Errorf("err = %v, want ErrManifestInvalid", err)
	}
}

func TestParse_NameValidation(t *testing.T) {
	cases := []struct {
		name string
		want bool // true if Parse should succeed
	}{
		{"hello-world", true},
		{"_system", true},
		{"hello_world_123", true},
		{"PDF-Bad", true}, // mixed case is fine, agentskills.io spec only forbids charset/length
		{"with space", false},
		{"a/b", false},
		{"toolong-" + strings.Repeat("x", 64), false}, // > 64 chars
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "---\nname: " + tc.name + "\ndescription: x\nlicense: MIT\n---\n"
			_, err := Parse([]byte(src))
			if tc.want && err != nil {
				t.Errorf("Parse(%q) err = %v, want ok", tc.name, err)
			}
			if !tc.want && err == nil {
				t.Errorf("Parse(%q) err = nil, want failure", tc.name)
			}
		})
	}
}

func TestParse_MissingDescriptionRejected(t *testing.T) {
	src := `---
name: ok
license: MIT
---
`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("Parse(no-description) returned nil error")
	}
	if !errors.Is(err, ErrManifestInvalid) {
		t.Errorf("err = %v, want ErrManifestInvalid", err)
	}
}

func TestParse_DescriptionTooLong(t *testing.T) {
	src := "---\nname: ok\ndescription: " + strings.Repeat("x", 1025) + "\nlicense: MIT\n---\n"
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("Parse(too-long-description) returned nil error")
	}
}

func TestParse_AllowedToolsRequireProvider(t *testing.T) {
	src := `---
name: ok
description: x
license: MIT
allowed-tools:
  - tools: [foo.bar]
---
`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("Parse(no-provider) returned nil error")
	}
}

func TestParse_UnknownTopLevelKeysPreserved(t *testing.T) {
	src := `---
name: ok
description: x
license: MIT
metadata:
  hugen:
    intents: [test]
  future_field:
    something: 42
---
`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if _, ok := m.Metadata["future_field"]; !ok {
		t.Errorf("Metadata.future_field missing — unknown keys must be preserved")
	}
	if !contains(m.Hugen.Intents, "test") {
		t.Errorf("Hugen.Intents not extracted from metadata.hugen.intents")
	}
}

func TestParse_BOMTolerant(t *testing.T) {
	bom := []byte{0xEF, 0xBB, 0xBF}
	src := append(bom, []byte("---\nname: ok\ndescription: x\nlicense: MIT\n---\n")...)
	m, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse with BOM error: %v", err)
	}
	if m.Name != "ok" {
		t.Errorf("Name = %q, want ok", m.Name)
	}
}

func TestParse_MalformedYAMLRejected(t *testing.T) {
	src := `---
name: [this is a list, not a string]
description: x
---
`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("Parse(malformed) returned nil error")
	}
	if !errors.Is(err, ErrManifestInvalid) {
		t.Errorf("err = %v, want ErrManifestInvalid", err)
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
