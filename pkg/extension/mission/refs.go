package mission

import (
	"fmt"
	"strings"
)

// ResolveDependsOn renders the [Resolved depends_on] section a
// worker sees in its first message. For each ref in depends_on,
// looks up the handoff in store and emits a labeled block:
//
//	[Resolved depends_on]
//	<ref1 (role: X, status: ok)>
//	<body verbatim, indented>
//	<ref2 (...)>
//	<body verbatim>
//
// Refs not in the store surface as a parse error — the executor
// detects this at wave start (forward reference / typo) and refuses
// to spawn the dependent worker rather than handing it a corrupt
// section. Returning an empty string is valid: no dependencies.
//
// Body rendering chooses a sensible projection per kind:
//   - kind=handoff/synthesis: stringify Body (raw if string, JSON
//     encode otherwise).
//   - kind=plan/verdict: JSON-encode body.
//
// Phase A keeps it minimal — phase D extends with memory_summary
// preview lines and phase F adds capability gating per worker.
func ResolveDependsOn(deps []string, store *Handoffs) (string, error) {
	if len(deps) == 0 {
		return "", nil
	}
	if store == nil {
		return "", fmt.Errorf("depends_on resolution: handoffs store is nil")
	}
	var b strings.Builder
	b.WriteString("[Resolved depends_on]\n")
	for _, ref := range deps {
		h, ok := store.Get(ref)
		if !ok {
			return "", fmt.Errorf("depends_on: ref %q not in handoffs store", ref)
		}
		fmt.Fprintf(&b, "<%s (role: %s, status: %s)>\n", h.Ref, displayRole(h), h.Status)
		body := renderBodyForFirstMessage(h)
		if body != "" {
			b.WriteString(body)
			if !strings.HasSuffix(body, "\n") {
				b.WriteByte('\n')
			}
		}
	}
	return b.String(), nil
}

// RenderHandoffCatalog renders the [Available handoffs] section —
// names only, no bodies — so a worker can discover refs it might
// late-bind via mission:get_handoff. The catalog is the discovery
// mechanism for "any ref readable" (spec §0.4a); workers can't
// invent refs not in their catalog.
//
// Returns "" when the store is empty.
func RenderHandoffCatalog(store *Handoffs) string {
	if store == nil {
		return ""
	}
	entries := store.List()
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[Available handoffs]\n")
	for _, h := range entries {
		fmt.Fprintf(&b, "  - %s   (%s, %s)\n", h.Ref, displayRole(h), h.Status)
	}
	return b.String()
}

func displayRole(h Handoff) string {
	if h.Subagent.Role != "" {
		return h.Subagent.Role
	}
	if h.Subagent.Skill != "" {
		return h.Subagent.Skill
	}
	return "?"
}

func renderBodyForFirstMessage(h Handoff) string {
	if h.Body == nil {
		return ""
	}
	if s, ok := h.Body.(string); ok {
		return s
	}
	// Fallback: render as JSON. Phase B may swap in YAML for
	// human-readable handoff blocks.
	enc, err := jsonMarshalIndent(h.Body)
	if err != nil {
		return fmt.Sprintf("<unrenderable body: %v>", err)
	}
	return enc
}
