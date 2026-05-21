package compactor

import (
	"context"
	"log/slog"
	"time"

	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/model"
	skillpkg "github.com/hugr-lab/hugen/pkg/skill"
)

// BuildConfig produces the resolved [Config] the runtime hands
// to [NewExtension]. Layers operator YAML
// ([config.CompactorConfig]) over the extension-owned defaults
// ([DefaultConfig]) and projects the per-tier overlay map shape
// for the resolver.
//
// Unknown LLMIntent values fall back to [model.IntentSummarize]
// with a warn log — matches the runtime adapter's discipline
// for any out-of-set string-typed enum coming from YAML.
//
// Phase 5.2 γ — entrypoint kept in the extension package so the
// runtime adapter is a thin one-liner and operator config /
// resolver / projection logic live next to the consumer.
func BuildConfig(in config.CompactorConfig, logger *slog.Logger) Config {
	cfg := DefaultConfig()

	if in.Strategy != "" {
		cfg.Strategy = resolveStrategy(in.Strategy, logger)
	}
	if in.WindowSize > 0 {
		cfg.WindowSize = in.WindowSize
	}
	if in.Enabled != nil {
		cfg.Enabled = *in.Enabled
		// Legacy shim: `enabled: false` maps to StrategyOff so
		// operators that haven't migrated to the strategy field
		// keep the same behaviour. Removed after one release per
		// spec §8.
		if !cfg.Enabled && in.Strategy == "" {
			cfg.Strategy = StrategyOff
		}
	}
	if in.MaxTurns > 0 {
		cfg.MaxTurns = in.MaxTurns
	}
	if in.MaxTokens > 0 {
		cfg.MaxTokens = in.MaxTokens
	}
	if in.PreservedRecentTurns > 0 {
		cfg.PreservedRecentTurns = in.PreservedRecentTurns
	}
	if in.DigestMaxTokens > 0 {
		cfg.DigestMaxTokens = in.DigestMaxTokens
	}
	if in.KeptVerbatimMax > 0 {
		cfg.KeptVerbatimMax = in.KeptVerbatimMax
	}
	if in.MinTurnGap > 0 {
		cfg.MinTurnGap = in.MinTurnGap
	}
	if in.LLMTimeoutMs > 0 {
		cfg.LLMTimeout = time.Duration(in.LLMTimeoutMs) * time.Millisecond
	}
	if in.LLMIntent != "" {
		cfg.LLMIntent = resolveCompactorIntent(in.LLMIntent, logger)
	}
	if in.TokenBudgetRatio > 0 {
		cfg.TokenBudgetRatio = in.TokenBudgetRatio
	}
	if in.UIMarker.Enabled != nil {
		cfg.UIMarkerEnabled = *in.UIMarker.Enabled
	}

	if len(in.Tiers) > 0 {
		cfg.Tiers = make(map[string]TierOverride, len(in.Tiers))
		for tier, t := range in.Tiers {
			cfg.Tiers[tier] = projectTierOverride(t, logger)
		}
	}

	return cfg
}

// resolveStrategy maps a YAML strategy string onto the
// extension-internal enum. Unknown values fall back to
// [StrategySummarize] with a warn log — same discipline as
// [resolveCompactorIntent]. Phase 5.2.η.
func resolveStrategy(s string, logger *slog.Logger) Strategy {
	st := Strategy(s)
	if ValidStrategy(st) {
		return st
	}
	if logger != nil {
		logger.Warn("compactor: unknown strategy in agent_config.yaml; falling back to summarize",
			"strategy", s)
	}
	return StrategySummarize
}

// resolveCompactorIntent maps an operator-supplied YAML intent
// string to [model.Intent]. Unknown values fall back to
// [model.IntentSummarize] and log a warn — operators see the
// problem at boot, the compactor keeps working with the
// conservative default.
func resolveCompactorIntent(s string, logger *slog.Logger) model.Intent {
	switch model.Intent(s) {
	case model.IntentDefault, model.IntentCheap,
		model.IntentToolCalling, model.IntentSummarize:
		return model.Intent(s)
	}
	if logger != nil {
		logger.Warn("compactor: unknown llm_intent in agent_config.yaml; falling back to summarize",
			"intent", s)
	}
	return model.IntentSummarize
}

// projectTierOverride mirrors [config.CompactorTier] onto
// [TierOverride]. LLMTimeoutMs / LLMIntent get projected through
// the same translators as the top-level path so a tier-scoped
// unknown intent emits the same warn shape.
func projectTierOverride(t config.CompactorTier, logger *slog.Logger) TierOverride {
	out := TierOverride{
		Enabled:              t.Enabled,
		MaxTurns:             t.MaxTurns,
		MaxTokens:            t.MaxTokens,
		PreservedRecentTurns: t.PreservedRecentTurns,
		DigestMaxTokens:      t.DigestMaxTokens,
		KeptVerbatimMax:      t.KeptVerbatimMax,
		MinTurnGap:           t.MinTurnGap,
		TokenBudgetRatio:     t.TokenBudgetRatio,
		WindowSize:           t.WindowSize,
	}
	if t.Strategy != nil {
		s := resolveStrategy(*t.Strategy, logger)
		out.Strategy = &s
	}
	if t.LLMTimeoutMs != nil {
		d := time.Duration(*t.LLMTimeoutMs) * time.Millisecond
		out.LLMTimeout = &d
	}
	if t.LLMIntent != nil {
		i := resolveCompactorIntent(*t.LLMIntent, logger)
		out.LLMIntent = &i
	}
	return out
}

// SkillManagerCatalog adapts a [*skillpkg.SkillManager] to the
// [SkillCatalog] surface. Looks up the dispatching skill
// (mission-level override) and, when role is non-empty, the
// matching sub_agents entry (per-role override). Returns
// (nil, nil, nil) when the skill / role is missing or carries
// no Compactor block — the resolver treats absent overrides as
// a no-op so the upstream layers stand.
type SkillManagerCatalog struct {
	manager *skillpkg.SkillManager
}

// NewSkillManagerCatalog constructs the adapter. nil manager is
// tolerated — LookupCompactor short-circuits to (nil, nil, nil)
// so fixtures without a SkillManager wired stay correct.
func NewSkillManagerCatalog(m *skillpkg.SkillManager) SkillCatalog {
	return &SkillManagerCatalog{manager: m}
}

// LookupCompactor implements [SkillCatalog].
func (c *SkillManagerCatalog) LookupCompactor(ctx context.Context, skillName, role string) (*OverrideSpec, *OverrideSpec, error) {
	if c == nil || c.manager == nil || skillName == "" {
		return nil, nil, nil
	}
	sk, err := c.manager.Get(ctx, skillName)
	if err != nil {
		return nil, nil, err
	}
	mission := projectCompactorOverride(sk.Manifest.Hugen.Compactor)
	var roleOv *OverrideSpec
	if role != "" {
		for i := range sk.Manifest.Hugen.SubAgents {
			if sk.Manifest.Hugen.SubAgents[i].Name != role {
				continue
			}
			roleOv = projectCompactorOverride(sk.Manifest.Hugen.SubAgents[i].Compactor)
			break
		}
	}
	return mission, roleOv, nil
}

// projectCompactorOverride mirrors the skill-manifest pointer
// shape onto the extension-internal wire type. nil input → nil
// output so the resolver can short-circuit on "no override".
func projectCompactorOverride(in *skillpkg.CompactorOverride) *OverrideSpec {
	if in == nil {
		return nil
	}
	return &OverrideSpec{
		Strategy:             in.Strategy,
		WindowSize:           in.WindowSize,
		Enabled:              in.Enabled,
		MaxTurns:             in.MaxTurns,
		MaxTokens:            in.MaxTokens,
		PreservedRecentTurns: in.PreservedRecentTurns,
		DigestMaxTokens:      in.DigestMaxTokens,
		KeptVerbatimMax:      in.KeptVerbatimMax,
		MinTurnGap:           in.MinTurnGap,
		LLMTimeoutMs:         in.LLMTimeoutMs,
		LLMIntent:            in.LLMIntent,
		TokenBudgetRatio:     in.TokenBudgetRatio,
	}
}
