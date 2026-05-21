// Package liveview ε — context budget aggregator. Pulls
// per-dimension token estimates from every contributor on the
// session and lands them in one payload so adapters render the
// pane without cross-extension knowledge.
package liveview

import (
	"context"
	"encoding/json"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// ContextBudget is the wire shape adapters consume to render
// the context-budget sidebar. All fields are optional — a fresh
// session emits a payload with zero values until the first turn
// reports usage. The payload composes:
//
//   - History size: from the [extension.HistoryOwner]
//     extension's reported history_tokens (compactor today).
//   - Session-cumulative model spend (prompt + completion):
//     from [SessionState.SessionUsage].
//   - Tool catalogue: from [SessionState.ToolCatalogTokens].
//   - Per-extension advertise breakdown: each StatusReporter's
//     payload may carry an `advertise_tokens` field; the
//     skill extension also exposes loaded vs catalogue splits.
//
// Phase 5.2 (context-budget ε).
type ContextBudget struct {
	// HistoryTokens is the model-visible history size in
	// EstimateTokens units (Block C excluded; it sits in
	// `Extensions.compactor` instead). 0 before the first
	// projected frame lands.
	HistoryTokens int `json:"history_tokens,omitempty"`

	// SessionUsage is the running total of prompt +
	// completion tokens reported by the model provider over
	// every retired turn. nil before any turn reports usage.
	SessionUsage *protocol.TokenUsage `json:"session_usage,omitempty"`

	// ToolsTokens is the [SessionState.ToolCatalogTokens]
	// snapshot — Name + Description + ArgSchema bytes across
	// every Tool the model receives this turn.
	ToolsTokens int `json:"tools_tokens,omitempty"`

	// Extensions maps extension name → advertise tokens.
	// Only extensions that surfaced a non-zero
	// `advertise_tokens` field in their ReportStatus
	// contribution appear here.
	Extensions map[string]int `json:"extensions,omitempty"`

	// Skills carries the skill extension's loaded vs catalogue
	// split. Nil when the skill extension hasn't reported a
	// non-zero split.
	Skills *SkillsBudget `json:"skills,omitempty"`
}

// SkillsBudget is the loaded vs catalogue split the skill
// extension reports separately so adapters can render "you've
// loaded N kB of skill bodies; the catalogue itself costs M kB".
type SkillsBudget struct {
	LoadedTokens    int `json:"loaded_tokens,omitempty"`
	AvailableTokens int `json:"available_tokens,omitempty"`
}

// buildContextBudget assembles the ContextBudget from the
// session's SessionState surface + every extension's StatusReporter
// contribution. Returns nil when every dimension is zero so
// adapters can skip rendering an empty pane.
//
// Implementation choice: decode each extension's status payload
// as a loose map[string]any and pull known keys
// (`advertise_tokens`, `history_tokens`, `loaded_skill_tokens`,
// `available_skill_tokens`). The convention is documented at
// each contributor's ReportStatus impl; mismatched keys are
// silently skipped — adapters render whatever lands.
func buildContextBudget(state extension.SessionState, exts map[string]json.RawMessage) *ContextBudget {
	if state == nil {
		return nil
	}
	budget := &ContextBudget{
		ToolsTokens:  state.ToolCatalogTokens(context.Background()),
		SessionUsage: state.SessionUsage(),
	}

	for name, raw := range exts {
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			continue
		}
		if v, ok := intField(doc, "history_tokens"); ok && v > 0 {
			// Compactor reports the history projection size.
			// HistoryOwner is the only contributor today, but
			// taking max here keeps the field correct if a
			// future extension also reports a history view.
			if v > budget.HistoryTokens {
				budget.HistoryTokens = v
			}
		}
		// Skill extension's loaded / catalogue split. When the
		// split is reported, it supersedes the legacy
		// advertise_tokens total so the UI doesn't render the
		// same number twice.
		loaded, hasLoaded := intField(doc, "loaded_skill_tokens")
		catalog, hasCatalog := intField(doc, "available_skill_tokens")
		if hasLoaded || hasCatalog {
			if budget.Skills == nil {
				budget.Skills = &SkillsBudget{}
			}
			if hasLoaded {
				budget.Skills.LoadedTokens = loaded
			}
			if hasCatalog {
				budget.Skills.AvailableTokens = catalog
			}
		} else if v, ok := intField(doc, "advertise_tokens"); ok && v > 0 {
			if budget.Extensions == nil {
				budget.Extensions = map[string]int{}
			}
			budget.Extensions[name] = v
		}
	}

	if budget.HistoryTokens == 0 && budget.SessionUsage == nil &&
		budget.ToolsTokens == 0 && len(budget.Extensions) == 0 &&
		budget.Skills == nil {
		return nil
	}
	return budget
}

// intField pulls a JSON-decoded integer-typed value out of a
// loose `map[string]any` document. JSON numbers land as float64
// after Unmarshal — coerce back to int.
func intField(doc map[string]any, key string) (int, bool) {
	v, ok := doc[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}
