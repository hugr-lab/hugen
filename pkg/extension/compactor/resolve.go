package compactor

import (
	"context"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/skill"
)

// resolveTierConfig returns the fully-resolved [Config] for the
// session-state's active tier with skill-manifest overrides
// layered on top. Resolution order (last layer wins, per-field):
//
//  1. baseline from [Extension.baseConfig] — the [config.CompactorView]
//     snapshot when present (operator agent_config.yaml's
//     `compactor:` block, with γ-defaults applied), else the
//     static [Config] supplied via [NewExtensionWithConfig],
//     else [DefaultConfig].
//  2. per-tier overlay from the baseline's Tiers map at
//     tier = skill.TierFromDepth(state.Depth()).
//  3. mission-level skill override, looked up via
//     [Deps.SkillCatalog] using [SessionState.Skill] +
//     [SessionState.Role] (skill is the dispatcher; the role lookup
//     side is ignored at this layer — it's the wider override).
//  4. per-role override from the same manifest, applied only when
//     [SessionState.Role] is non-empty.
//
// Catalog lookup failures degrade gracefully — the resolver
// returns the configuration after the layers it succeeded in
// applying. The compaction pipeline is never blocked on catalog
// I/O.
//
// Phase 5.2 γ — see
// design/004-runtime-post-phase-i/phase-5.2-compactor-spec.md §4.4.
func (e *Extension) resolveTierConfig(ctx context.Context, state extension.SessionState) Config {
	cfg := e.baseConfig() // fresh snapshot per resolve; supports future hot-reload

	if state == nil {
		return cfg
	}

	tier := skill.TierFromDepth(state.Depth())
	if t, ok := cfg.Tiers[tier]; ok {
		applyTierOverride(&cfg, t)
	}

	if e.deps.SkillCatalog == nil {
		return cfg
	}

	missionOv, roleOv, err := e.deps.SkillCatalog.LookupCompactor(ctx, state.Skill(), state.Role())
	if err != nil {
		// Lookup failure is treated as "no overrides" — log debug
		// but never block compaction on catalog problems.
		e.logger.Debug("compactor resolve: catalog lookup failed",
			"session", state.SessionID(), "err", err)
		return cfg
	}
	if missionOv != nil {
		applyOverrideSpec(&cfg, *missionOv)
	}
	if roleOv != nil && state.Role() != "" {
		applyOverrideSpec(&cfg, *roleOv)
	}
	return cfg
}

// applyTierOverride overwrites cfg fields with explicit values
// from t. nil pointers leave the corresponding field untouched.
// Modifies cfg in place; caller passes a pointer into its own
// stack-allocated value copy.
func applyTierOverride(cfg *Config, t TierOverride) {
	if t.Strategy != nil {
		cfg.Strategy = *t.Strategy
	}
	if t.WindowSize != nil {
		cfg.WindowSize = *t.WindowSize
	}
	if t.Enabled != nil {
		cfg.Enabled = *t.Enabled
	}
	if t.MaxTurns != nil {
		cfg.MaxTurns = *t.MaxTurns
	}
	if t.MaxTokens != nil {
		cfg.MaxTokens = *t.MaxTokens
	}
	if t.PreservedRecentTurns != nil {
		cfg.PreservedRecentTurns = *t.PreservedRecentTurns
	}
	if t.DigestMaxTokens != nil {
		cfg.DigestMaxTokens = *t.DigestMaxTokens
	}
	if t.KeptVerbatimMax != nil {
		cfg.KeptVerbatimMax = *t.KeptVerbatimMax
	}
	if t.MinTurnGap != nil {
		cfg.MinTurnGap = *t.MinTurnGap
	}
	if t.LLMTimeout != nil {
		cfg.LLMTimeout = *t.LLMTimeout
	}
	if t.LLMIntent != nil {
		cfg.LLMIntent = *t.LLMIntent
	}
	if t.TokenBudgetRatio != nil {
		cfg.TokenBudgetRatio = *t.TokenBudgetRatio
	}
}

// applyOverrideSpec is the skill-manifest variant of
// applyTierOverride. The fields are the same set; the only
// shape difference is LLMTimeoutMs (int) and LLMIntent (string)
// which the spec carries pre-typed so pkg/skill stays
// duration-agnostic.
func applyOverrideSpec(cfg *Config, o OverrideSpec) {
	if o.Strategy != nil {
		// Unknown values silently inherit (parity with
		// resolveIntent below). Operator-level validation lives in
		// the runtime config adapter (BuildConfig).
		if st := Strategy(*o.Strategy); ValidStrategy(st) {
			cfg.Strategy = st
		}
	}
	if o.WindowSize != nil {
		cfg.WindowSize = *o.WindowSize
	}
	if o.Enabled != nil {
		cfg.Enabled = *o.Enabled
	}
	if o.MaxTurns != nil {
		cfg.MaxTurns = *o.MaxTurns
	}
	if o.MaxTokens != nil {
		cfg.MaxTokens = *o.MaxTokens
	}
	if o.PreservedRecentTurns != nil {
		cfg.PreservedRecentTurns = *o.PreservedRecentTurns
	}
	if o.DigestMaxTokens != nil {
		cfg.DigestMaxTokens = *o.DigestMaxTokens
	}
	if o.KeptVerbatimMax != nil {
		cfg.KeptVerbatimMax = *o.KeptVerbatimMax
	}
	if o.MinTurnGap != nil {
		cfg.MinTurnGap = *o.MinTurnGap
	}
	if o.LLMTimeoutMs != nil {
		cfg.LLMTimeout = time.Duration(*o.LLMTimeoutMs) * time.Millisecond
	}
	if o.LLMIntent != nil {
		cfg.LLMIntent = resolveIntent(*o.LLMIntent, cfg.LLMIntent)
	}
	if o.TokenBudgetRatio != nil {
		cfg.TokenBudgetRatio = *o.TokenBudgetRatio
	}
}

// resolveIntent maps a YAML intent string to a [model.Intent].
// Empty input returns the fallback (typically the prior layer's
// value or [model.IntentSummarize]). Unknown strings also fall
// back — the runtime adapter logs at boot time when the operator-
// level intent is unknown; here we silently inherit, which is the
// right default for a per-skill override (a typo shouldn't
// silently disable a working summariser intent).
func resolveIntent(s string, fallback model.Intent) model.Intent {
	switch model.Intent(s) {
	case model.IntentDefault, model.IntentCheap,
		model.IntentToolCalling, model.IntentSummarize:
		return model.Intent(s)
	}
	if s == "" {
		return fallback
	}
	return fallback
}
