package skill

import (
	"fmt"
	"sort"
	"strings"
)

// RenderInputsSchemaBlock formats a JSON Schema's top-level
// `properties` + `required` list into a compact prose block the
// runtime injects into Available missions / Available tasks prompt
// sections.
//
// Output shape (indented under indent="  "):
//
//	  inputs (required):
//	    foo (string) — desc.
//	    bar (integer)
//	  inputs (optional):
//	    baz (object) — desc.
//
// Returns an empty string when the schema is nil, has no
// `properties` map, or carries no string-keyed entries — caller
// suppresses the block entirely in that case so a schema-less
// recipe / mission keeps its compact bare summary.
//
// Only the top-level `properties` + `required` keys are read. Nested
// schemas (objects within objects) pass through as `(object)` —
// surfacing their inner shape would bloat the prompt; root can call
// `skill:ref` for the full schema if it ever needs it. Phase 6.1d.
func RenderInputsSchemaBlock(schema map[string]any, indent string) string {
	if len(schema) == 0 {
		return ""
	}
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return ""
	}
	required := requiredKeySet(schema["required"])
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var req, opt []string
	for _, k := range keys {
		entry := formatProperty(k, props[k])
		if _, isReq := required[k]; isReq {
			req = append(req, entry)
		} else {
			opt = append(opt, entry)
		}
	}

	var b strings.Builder
	innerIndent := indent + "  "
	if len(req) > 0 {
		fmt.Fprintf(&b, "%sinputs (required):\n", indent)
		for _, e := range req {
			fmt.Fprintf(&b, "%s%s\n", innerIndent, e)
		}
	}
	if len(opt) > 0 {
		fmt.Fprintf(&b, "%sinputs (optional):\n", indent)
		for _, e := range opt {
			fmt.Fprintf(&b, "%s%s\n", innerIndent, e)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// requiredKeySet flattens the schema's `required` value (either
// []string or []any of strings, depending on how the YAML was
// unmarshalled) into a presence-set for O(1) lookups.
func requiredKeySet(v any) map[string]struct{} {
	out := map[string]struct{}{}
	switch list := v.(type) {
	case []string:
		for _, k := range list {
			if k != "" {
				out[k] = struct{}{}
			}
		}
	case []any:
		for _, item := range list {
			if k, ok := item.(string); ok && k != "" {
				out[k] = struct{}{}
			}
		}
	}
	return out
}

// formatProperty renders one property entry as `name (type) — desc`.
// The `(type)` is omitted when absent; the ` — desc` is omitted when
// the property carries no description string.
func formatProperty(name string, raw any) string {
	prop, _ := raw.(map[string]any)
	typ, _ := prop["type"].(string)
	desc, _ := prop["description"].(string)
	desc = strings.TrimSpace(desc)

	var b strings.Builder
	b.WriteString(name)
	if typ != "" {
		fmt.Fprintf(&b, " (%s)", typ)
	}
	if desc != "" {
		fmt.Fprintf(&b, " — %s", collapseWhitespace(desc))
	}
	return b.String()
}

// collapseWhitespace replaces internal newlines + tabs + runs of
// spaces with single spaces so a wrapped YAML description renders
// on one line in the prompt block.
func collapseWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' || r == ' ' {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	return strings.TrimSpace(b.String())
}
