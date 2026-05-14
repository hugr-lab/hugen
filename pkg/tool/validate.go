// LLM-tool JSON-Schema conformance — both validation (fail-fast for
// our own tools whose schemas we control) and sanitisation (silent
// repair of upstream vendored MCP tools whose schemas we don't).
//
// Conservative subset that all chat-completion providers we route
// through Hugr (Anthropic, OpenAI, Gemini) accept — see
// query-engine/integration-test/models for the canonical shape:
//   - Top-level shape must be `{"type":"object", ...}`.
//   - `type:"array"` properties must declare `items`.
//   - `additionalProperties` is rejected by Gemini.
//   - `$ref` / `oneOf` / `anyOf` / `allOf` are rejected by Gemini.
//
// Use ValidateLLMSchema for our own static schemas (we want to break
// the build on regression). Use SanitizeLLMSchema at the MCPProvider
// boundary — vendored tools get auto-cleaned and a warning is logged
// if anything changed.
package tool

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ValidateLLMSchema returns nil if raw is empty or describes a JSON
// Schema acceptable to all chat-completion providers we route
// through Hugr. Empty/zero schemas are treated as "object with no
// properties"; callers fill that in at marshal time.
func ValidateLLMSchema(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var node any
	if err := json.Unmarshal(raw, &node); err != nil {
		return fmt.Errorf("schema: invalid JSON: %w", err)
	}
	obj, ok := node.(map[string]any)
	if !ok {
		return fmt.Errorf("schema: top-level must be an object, got %T", node)
	}
	if t, ok := obj["type"].(string); !ok || t != "object" {
		return fmt.Errorf("schema: top-level type must be \"object\" (got %v)", obj["type"])
	}
	return validateNode(obj, "")
}

// validateNode recurses into one schema node. path is a JSON-Pointer-ish
// trail for diagnostics ("properties.args.items").
func validateNode(node map[string]any, path string) error {
	for _, banned := range []string{"additionalProperties", "$ref", "oneOf", "anyOf", "allOf"} {
		if _, exists := node[banned]; exists {
			return fmt.Errorf("schema: %q is not supported (at %s)", banned, where(path))
		}
	}
	t, _ := node["type"].(string)
	if t == "array" {
		items, ok := node["items"]
		if !ok {
			return fmt.Errorf("schema: array property missing \"items\" (at %s)", where(path))
		}
		if itemsObj, ok := items.(map[string]any); ok {
			if err := validateNode(itemsObj, join(path, "items")); err != nil {
				return err
			}
		}
	}
	if props, ok := node["properties"].(map[string]any); ok {
		for name, raw := range props {
			child, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if err := validateNode(child, join(path, "properties."+name)); err != nil {
				return err
			}
		}
	}
	return nil
}

func join(path, segment string) string {
	if path == "" {
		return segment
	}
	return path + "." + segment
}

func where(path string) string {
	if path == "" {
		return "<root>"
	}
	return path
}

// SanitizeLLMSchema returns a cleaned copy of raw conforming to the
// conservative subset, plus a list of human-readable repair notes
// describing every change. nil schemas pass through unchanged.
//
// Repairs applied:
//   - Drop `additionalProperties` / `$ref` / `oneOf` / `anyOf` /
//     `allOf` wherever they appear.
//   - Force top-level `type` to `"object"` (some upstream tools
//     omit it).
//   - For `type:"array"` properties without `items`, inject
//     `"items": {}` (accepts any element).
//
// Returns the original bytes verbatim when no repair was needed.
func SanitizeLLMSchema(raw json.RawMessage) (json.RawMessage, []string, error) {
	if len(raw) == 0 {
		return raw, nil, nil
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return raw, nil, nil
	}
	var node any
	if err := json.Unmarshal(raw, &node); err != nil {
		return nil, nil, fmt.Errorf("schema: invalid JSON: %w", err)
	}
	obj, ok := node.(map[string]any)
	if !ok {
		// Non-object top-level — wrap in a permissive object so the
		// upstream provider sees a shape it accepts.
		return json.RawMessage(`{"type":"object"}`),
			[]string{"top-level not an object: replaced with empty object"}, nil
	}
	notes := make([]string, 0, 4)
	if t, ok := obj["type"].(string); !ok || t != "object" {
		obj["type"] = "object"
		notes = append(notes, "top-level type forced to \"object\"")
	}
	sanitizeNode(obj, "", &notes)
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, notes, fmt.Errorf("schema: re-marshal sanitized form: %w", err)
	}
	if len(notes) == 0 {
		// Identical content; return original bytes to keep hashes
		// stable.
		return raw, nil, nil
	}
	return out, notes, nil
}

func sanitizeNode(node map[string]any, path string, notes *[]string) {
	// "examples" landed in phase 5.1 to help weak local models
	// (Gemma) infer the envelope shape of `session:inquire`, but
	// Gemini's function-declaration schema parser rejects unknown
	// fields with an INVALID_ARGUMENT. Strip it project-wide so
	// any extension contributing a schema with examples doesn't
	// silently break the Gemini transport.
	for _, banned := range []string{"additionalProperties", "$ref", "oneOf", "anyOf", "allOf", "examples"} {
		if _, exists := node[banned]; exists {
			delete(node, banned)
			*notes = append(*notes, fmt.Sprintf("dropped %q at %s", banned, where(path)))
		}
	}
	if t, _ := node["type"].(string); t == "array" {
		if _, ok := node["items"]; !ok {
			node["items"] = map[string]any{}
			*notes = append(*notes, fmt.Sprintf("injected empty items at %s", where(path)))
		}
		if itemsObj, ok := node["items"].(map[string]any); ok {
			sanitizeNode(itemsObj, join(path, "items"), notes)
		}
	}
	if props, ok := node["properties"].(map[string]any); ok {
		for name, raw := range props {
			child, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			sanitizeNode(child, join(path, "properties."+name), notes)
		}
	}
}
