package skill

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/oasdiff/yaml"
)

// Manifest is the parsed agentskills.io frontmatter plus hugen
// extensions extracted from metadata.hugen.*. Unknown top-level
// keys are preserved in Metadata so skills authored against a
// future spec revision still parse.
type Manifest struct {
	Name          string         `json:"name"`
	Description   string         `json:"description"`
	License       string         `json:"license"`
	Compatibility Compatibility  `json:"compatibility,omitempty"`
	AllowedTools  AllowedTools   `json:"allowed-tools,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`

	// Hugen is the typed projection of metadata.hugen.* — populated
	// after Parse runs the raw YAML through extractHugen. Authors
	// don't write Hugen directly; they put the data under
	// metadata.hugen in the manifest.
	Hugen HugenMetadata `json:"-"`

	// Body is the SKILL.md content after the closing `---`. Empty
	// when the file is frontmatter-only.
	Body []byte `json:"-"`

	// Raw is the original frontmatter bytes (between the two `---`
	// lines, exclusive). Useful for re-emitting / round-trip tests.
	Raw []byte `json:"-"`
}

// Compatibility ties a skill to the model/runtime it was authored
// against. Both fields are optional — agentskills.io recommends
// them but doesn't require them.
type Compatibility struct {
	Model   string `json:"model,omitempty"`
	Runtime string `json:"runtime,omitempty"`
}

// ToolGrant declares which tools of a given provider the skill
// makes available to the LLM when loaded.
type ToolGrant struct {
	Provider string   `json:"provider"`
	Tools    []string `json:"tools"`
}

// AllowedTools is the parsed allowed-tools list. Two manifest
// notations decode into the same []ToolGrant:
//
//	# grouped — agentskills.io spec form
//	allowed-tools:
//	  - provider: bash-mcp
//	    tools: [bash.run, bash.read_file]
//
//	# flat — hugen shorthand, "provider:tool" entries
//	allowed-tools:
//	  - bash-mcp:bash.run
//	  - bash-mcp:bash.read_file
//	  - system:skill_load
//
// Mixed lists are supported. Flat entries that share a provider
// merge into a single ToolGrant for that provider.
type AllowedTools []ToolGrant

// UnmarshalJSON decodes either notation into the same []ToolGrant
// shape. agentskills.io strict mode still gets a 1:1 round-trip
// because grouped entries are decoded directly into ToolGrant.
func (a *AllowedTools) UnmarshalJSON(data []byte) error {
	if len(bytes.TrimSpace(data)) == 0 || string(bytes.TrimSpace(data)) == "null" {
		*a = nil
		return nil
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("allowed-tools: must be a list: %w", err)
	}
	out := make([]ToolGrant, 0, len(raw))
	byProvider := map[string]int{} // provider → index in out
	for i, item := range raw {
		trimmed := bytes.TrimSpace(item)
		if len(trimmed) == 0 {
			return fmt.Errorf("allowed-tools[%d]: empty entry", i)
		}
		switch trimmed[0] {
		case '"':
			var entry string
			if err := json.Unmarshal(item, &entry); err != nil {
				return fmt.Errorf("allowed-tools[%d]: %w", i, err)
			}
			j := strings.Index(entry, ":")
			if j <= 0 || j == len(entry)-1 {
				return fmt.Errorf(`allowed-tools[%d]: %q must be "provider:tool"`, i, entry)
			}
			provider, name := entry[:j], entry[j+1:]
			if idx, ok := byProvider[provider]; ok {
				out[idx].Tools = append(out[idx].Tools, name)
			} else {
				byProvider[provider] = len(out)
				out = append(out, ToolGrant{Provider: provider, Tools: []string{name}})
			}
		case '{':
			var grant ToolGrant
			if err := json.Unmarshal(item, &grant); err != nil {
				return fmt.Errorf("allowed-tools[%d]: %w", i, err)
			}
			out = append(out, grant)
			// Don't merge object-form entries into byProvider — the
			// author may intentionally split a provider across two
			// blocks (e.g. group commentary), and we want the
			// round-trip to preserve that shape.
		default:
			return fmt.Errorf("allowed-tools[%d]: expected string or object, got %s", i, string(trimmed))
		}
	}
	*a = out
	return nil
}

// HugenMetadata is the typed view of metadata.hugen.* — the
// hugen-specific extensions inside the manifest.
type HugenMetadata struct {
	Requires  []string                  `json:"requires,omitempty"`
	Intents   []string                  `json:"intents,omitempty"`
	SubAgents []SubAgentRole            `json:"sub_agents,omitempty"`
	Memory    map[string]MemoryCategory `json:"memory,omitempty"`
	// MaxTurns is the per-skill cap on the model→tool→model loop
	// inside a single user turn. Different skills warrant different
	// budgets — explorer/analyst skills routinely need 25+ tool
	// turns, while a quick-task skill may want a tight 3 to fail
	// fast. The runtime takes the max across loaded skills; 0 (or
	// absent) defers to the runtime default (defaultMaxToolIterations,
	// currently 15).
	MaxTurns int `json:"max_turns,omitempty" yaml:"max_turns,omitempty"`

	// Autoload, when true, tells the SessionManager to load this
	// skill into every newly opened session whose type appears in
	// AutoloadFor. Loading is idempotent — manual /skill load of
	// the same name is a no-op.
	Autoload bool `json:"autoload,omitempty" yaml:"autoload,omitempty"`

	// AutoloadFor is the list of session types in which Autoload
	// fires. Recognised values:
	//   - "root"     — sessions where the user talks to the main
	//                  agent (the only kind in phase 3).
	//   - "subagent" — sessions where the main agent talks to a
	//                  spawned sub-agent (phase 4).
	// Empty defaults to ["root"] — the conservative behaviour that
	// keeps autoload skills out of sub-agent sessions until an
	// author opts in.
	AutoloadFor []string `json:"autoload_for,omitempty" yaml:"autoload_for,omitempty"`
}

// AutoloadIn reports whether the manifest opts into autoload for
// the given session type. Resolves the empty-AutoloadFor default
// ([root]) so callers don't repeat the rule.
func (m *Manifest) AutoloadIn(sessionType string) bool {
	if !m.Hugen.Autoload {
		return false
	}
	if len(m.Hugen.AutoloadFor) == 0 {
		return sessionType == SessionTypeRoot
	}
	for _, t := range m.Hugen.AutoloadFor {
		if t == sessionType {
			return true
		}
	}
	return false
}

// SessionType labels for Manifest.AutoloadFor entries.
const (
	SessionTypeRoot     = "root"
	SessionTypeSubAgent = "subagent"
)

// SubAgentRole is the manifest shape phase-3 validates and
// phase-4 will dispatch.
type SubAgentRole struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Tools       []ToolGrant `json:"tools,omitempty"`
}

// MemoryCategory ports the legacy memory.yaml shape.
type MemoryCategory struct {
	TTL         string `json:"ttl,omitempty"`
	MaxItems    int    `json:"max_items,omitempty"`
	SummariseAt int    `json:"summarise_at,omitempty"`
}

var (
	// agentskills.io: name is [A-Za-z0-9_-]{1,64}.
	nameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

	// Frontmatter delimiter — three or more dashes on a line by
	// themselves. The spec uses exactly three; we accept any count
	// ≥3 to be lenient with typos that round-trip through some
	// editors.
	delimRe = regexp.MustCompile(`(?m)^-{3,}\s*$`)
)

// Parse a SKILL.md (or just-frontmatter) byte slice into a
// Manifest. Returns ErrManifestInvalid (wrapped) when validation
// fails; the caller can errors.Is to detect that class.
func Parse(content []byte) (Manifest, error) {
	var m Manifest

	front, body, err := splitFrontmatter(content)
	if err != nil {
		return m, fmt.Errorf("%w: %v", ErrManifestInvalid, err)
	}
	m.Raw = front
	m.Body = body

	if len(front) == 0 {
		return m, fmt.Errorf("%w: empty frontmatter", ErrManifestInvalid)
	}
	if err := yaml.Unmarshal(front, &m); err != nil {
		return m, fmt.Errorf("%w: yaml: %v", ErrManifestInvalid, err)
	}

	if err := m.validate(); err != nil {
		return m, fmt.Errorf("%w: %v", ErrManifestInvalid, err)
	}

	hugen, err := extractHugen(m.Metadata)
	if err != nil {
		return m, fmt.Errorf("%w: %v", ErrManifestInvalid, err)
	}
	m.Hugen = hugen
	return m, nil
}

// ParseReader is a convenience wrapper around Parse for io.Reader.
func ParseReader(r io.Reader) (Manifest, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: read: %v", ErrManifestInvalid, err)
	}
	return Parse(b)
}

// splitFrontmatter returns (frontmatter-bytes, body-bytes). The
// frontmatter is the YAML between the opening `---` and the
// closing `---`; the body is everything after the closing `---`.
// A document with no closing delimiter is rejected. A document
// with no frontmatter at all returns empty frontmatter and the
// full content as body.
func splitFrontmatter(content []byte) (front, body []byte, err error) {
	// Skip BOM (U+FEFF, encoded as EF BB BF in UTF-8) and leading
	// whitespace before the opening delimiter.
	trimmed := bytes.TrimPrefix(content, []byte{0xEF, 0xBB, 0xBF})
	trimmed = bytes.TrimLeft(trimmed, " \t\r\n")
	if !bytes.HasPrefix(trimmed, []byte("---")) {
		return nil, content, nil
	}
	loc := delimRe.FindAllIndex(trimmed, 2)
	if len(loc) < 2 {
		return nil, nil, errors.New("frontmatter has no closing delimiter")
	}
	frontStart := loc[0][1]
	frontEnd := loc[1][0]
	front = bytes.TrimSpace(trimmed[frontStart:frontEnd])
	body = bytes.TrimLeft(trimmed[loc[1][1]:], "\r\n")
	return front, body, nil
}

func (m *Manifest) validate() error {
	if !nameRe.MatchString(m.Name) {
		return fmt.Errorf("name %q does not match [A-Za-z0-9_-]{1,64}", m.Name)
	}
	if m.Description == "" {
		return errors.New("description is required")
	}
	if len(m.Description) > 1024 {
		return fmt.Errorf("description length %d exceeds 1024 chars", len(m.Description))
	}
	for i, g := range m.AllowedTools {
		if g.Provider == "" {
			return fmt.Errorf("allowed-tools[%d].provider is required", i)
		}
	}
	return nil
}

// extractHugen pulls metadata.hugen.* into a typed HugenMetadata
// by re-marshalling that sub-tree through YAML. Cheaper than
// hand-walking and tolerates any YAML-shape any layer accepts.
func extractHugen(meta map[string]any) (HugenMetadata, error) {
	var out HugenMetadata
	if meta == nil {
		return out, nil
	}
	raw, ok := meta["hugen"]
	if !ok {
		return out, nil
	}
	encoded, err := yaml.Marshal(raw)
	if err != nil {
		return out, fmt.Errorf("metadata.hugen: marshal: %v", err)
	}
	if err := yaml.Unmarshal(encoded, &out); err != nil {
		return out, fmt.Errorf("metadata.hugen: unmarshal: %v", err)
	}
	return out, nil
}
