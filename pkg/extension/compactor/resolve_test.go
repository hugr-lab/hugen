package compactor

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/prompts"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// resolveStateFake is a minimal SessionState that carries a
// configurable Depth + Skill + Role so the per-tier / per-skill
// / per-role resolver can be exercised without spinning up a
// real session.
type resolveStateFake struct {
	id     string
	depth  int
	skill  string
	role   string
	values sync.Map
}

func (s *resolveStateFake) SessionID() string                              { return s.id }
func (s *resolveStateFake) SubagentName() string                           { return "" }
func (s *resolveStateFake) Role() string                                   { return s.role }
func (s *resolveStateFake) Skill() string                                  { return s.skill }
func (s *resolveStateFake) Depth() int                                     { return s.depth }
func (s *resolveStateFake) Tier() string                                   { return skill.TierFromDepth(s.depth) }
func (s *resolveStateFake) Parent() (extension.SessionState, bool)         { return nil, false }
func (s *resolveStateFake) Children() []extension.SessionState             { return nil }
func (s *resolveStateFake) Tools() *tool.ToolManager                       { return nil }
func (s *resolveStateFake) Prompts() *prompts.Renderer                     { return nil }
func (s *resolveStateFake) Value(name string) (any, bool)                  { v, ok := s.values.Load(name); return v, ok }
func (s *resolveStateFake) SetValue(name string, value any)                { s.values.Store(name, value) }
func (s *resolveStateFake) Emit(_ context.Context, _ protocol.Frame) error { return nil }
func (s *resolveStateFake) IsClosed() bool                                 { return false }
func (s *resolveStateFake) Submit(_ context.Context, _ protocol.Frame) <-chan struct{} {
	return nil
}
func (s *resolveStateFake) OutboxOnly(_ context.Context, _ protocol.Frame) error { return nil }
func (s *resolveStateFake) ToolCatalogTokens(_ context.Context) int               { return 0 }
func (s *resolveStateFake) SessionUsage() *protocol.TokenUsage                    { return nil }
func (s *resolveStateFake) Extensions() []extension.Extension                    { return nil }
func (s *resolveStateFake) RequestInquiry(_ context.Context, _ protocol.InquiryRequestPayload) (*protocol.InquiryResponse, error) {
	return nil, nil
}

// stubCatalog implements SkillCatalog and returns canned
// overrides per (skill, role) key.
type stubCatalog struct {
	missions map[string]*OverrideSpec
	roles    map[string]map[string]*OverrideSpec
	err      error
}

func (c *stubCatalog) LookupCompactor(_ context.Context, skill, role string) (*OverrideSpec, *OverrideSpec, error) {
	if c.err != nil {
		return nil, nil, c.err
	}
	var m, r *OverrideSpec
	if c.missions != nil {
		m = c.missions[skill]
	}
	if c.roles != nil {
		if byRole, ok := c.roles[skill]; ok {
			r = byRole[role]
		}
	}
	return m, r, nil
}

// Pointer helpers — keep tests readable.
func ptrBool(b bool) *bool                       { return &b }
func ptrInt(i int) *int                          { return &i }
func ptrFloat(f float64) *float64                { return &f }
func ptrDuration(d time.Duration) *time.Duration { return &d }

func TestResolveTierConfig_TopLevelOnly(t *testing.T) {
	// No tier overlay, no catalog — resolver returns the
	// extension's baseline Config verbatim.
	base := DefaultConfig()
	base.MaxTurns = 42
	e := NewExtensionWithConfig(slog.Default(), base, Deps{})
	st := &resolveStateFake{id: "s1", depth: 0}
	got := e.resolveTierConfig(context.Background(), st)
	if got.MaxTurns != 42 {
		t.Fatalf("MaxTurns = %d, want 42 (top-level baseline)", got.MaxTurns)
	}
	if !got.Enabled {
		t.Fatalf("Enabled = false, want true (default)")
	}
}

func TestResolveTierConfig_TierOverlay(t *testing.T) {
	base := DefaultConfig()
	base.MaxTurns = 50
	base.PreservedRecentTurns = 10
	base.Tiers = map[string]TierOverride{
		"root": {
			MaxTurns:             ptrInt(100),
			PreservedRecentTurns: ptrInt(20),
		},
		"worker": {
			Enabled: ptrBool(false),
		},
	}
	e := NewExtensionWithConfig(slog.Default(), base, Deps{})

	// Root tier (depth 0) — overlay applied.
	root := &resolveStateFake{id: "root", depth: 0}
	got := e.resolveTierConfig(context.Background(), root)
	if got.MaxTurns != 100 {
		t.Errorf("root MaxTurns = %d, want 100 (tier overlay)", got.MaxTurns)
	}
	if got.PreservedRecentTurns != 20 {
		t.Errorf("root PreservedRecentTurns = %d, want 20", got.PreservedRecentTurns)
	}
	if !got.Enabled {
		t.Errorf("root Enabled = false, want true (no override)")
	}

	// Worker tier (depth 2) — only Enabled overridden.
	worker := &resolveStateFake{id: "w", depth: 2}
	got = e.resolveTierConfig(context.Background(), worker)
	if got.Enabled {
		t.Errorf("worker Enabled = true, want false (tier overlay)")
	}
	if got.MaxTurns != 50 {
		t.Errorf("worker MaxTurns = %d, want 50 (top-level baseline)", got.MaxTurns)
	}

	// Mission tier (depth 1) — no overlay registered; baseline.
	mission := &resolveStateFake{id: "m", depth: 1}
	got = e.resolveTierConfig(context.Background(), mission)
	if got.MaxTurns != 50 {
		t.Errorf("mission MaxTurns = %d, want 50 (no tier overlay)", got.MaxTurns)
	}
	if !got.Enabled {
		t.Errorf("mission Enabled = false, want true (default)")
	}
}

func TestResolveTierConfig_MissionOverride_NarrowestWins(t *testing.T) {
	base := DefaultConfig()
	base.MaxTurns = 50
	base.PreservedRecentTurns = 10
	base.Tiers = map[string]TierOverride{
		"worker": {Enabled: ptrBool(false)}, // worker disabled by default
	}
	catalog := &stubCatalog{
		missions: map[string]*OverrideSpec{
			"long-running-mission": {
				Enabled:              ptrBool(true), // mission re-enables for its workers
				PreservedRecentTurns: ptrInt(15),
			},
		},
		roles: map[string]map[string]*OverrideSpec{
			"long-running-mission": {
				"repl-loop": {
					PreservedRecentTurns: ptrInt(20),
					MaxTurns:             ptrInt(30),
				},
			},
		},
	}
	e := NewExtensionWithConfig(slog.Default(), base, Deps{SkillCatalog: catalog})

	// Mission-level override applied (worker depth, skill set, no role).
	wkrNoRole := &resolveStateFake{id: "w0", depth: 2, skill: "long-running-mission", role: ""}
	got := e.resolveTierConfig(context.Background(), wkrNoRole)
	if !got.Enabled {
		t.Errorf("worker w/ mission override: Enabled = false, want true")
	}
	if got.PreservedRecentTurns != 15 {
		t.Errorf("worker PreservedRecentTurns = %d, want 15 (mission override)", got.PreservedRecentTurns)
	}
	if got.MaxTurns != 50 {
		t.Errorf("worker MaxTurns = %d, want 50 (untouched)", got.MaxTurns)
	}

	// Per-role override wins (narrowest layer).
	wkrRole := &resolveStateFake{id: "w1", depth: 2, skill: "long-running-mission", role: "repl-loop"}
	got = e.resolveTierConfig(context.Background(), wkrRole)
	if got.PreservedRecentTurns != 20 {
		t.Errorf("worker w/ role override: PreservedRecentTurns = %d, want 20", got.PreservedRecentTurns)
	}
	if got.MaxTurns != 30 {
		t.Errorf("worker w/ role override: MaxTurns = %d, want 30", got.MaxTurns)
	}
	if !got.Enabled {
		t.Errorf("worker w/ role override: Enabled = false, want true (inherited from mission)")
	}
}

func TestResolveTierConfig_NilSkillCatalogStillReturnsTopAndTier(t *testing.T) {
	// Verify nil SkillCatalog is the safe fallback: top-level
	// + tier overlay still apply, skill / role layers silently
	// skip.
	base := DefaultConfig()
	base.MaxTurns = 50
	base.Tiers = map[string]TierOverride{
		"mission": {MaxTurns: ptrInt(80)},
	}
	e := NewExtensionWithConfig(slog.Default(), base, Deps{}) // SkillCatalog nil

	st := &resolveStateFake{id: "m", depth: 1, skill: "any", role: "any"}
	got := e.resolveTierConfig(context.Background(), st)
	if got.MaxTurns != 80 {
		t.Fatalf("MaxTurns = %d, want 80 (mission overlay; catalog absent)", got.MaxTurns)
	}
}

func TestResolveTierConfig_CatalogErrorDegradesGracefully(t *testing.T) {
	// Catalog lookup failures are non-fatal: resolver returns
	// the config after the layers it succeeded in applying
	// (top-level + tier), never blocks compaction.
	base := DefaultConfig()
	base.MaxTurns = 50
	catalog := &stubCatalog{err: errors.New("boom")}
	e := NewExtensionWithConfig(slog.Default(), base, Deps{SkillCatalog: catalog})

	st := &resolveStateFake{id: "m", depth: 1, skill: "any", role: "any"}
	got := e.resolveTierConfig(context.Background(), st)
	if got.MaxTurns != 50 {
		t.Fatalf("MaxTurns = %d, want 50 (top-level after catalog error)", got.MaxTurns)
	}
}

func TestResolveTierConfig_IntentAndTimeoutOverrides(t *testing.T) {
	// Mission-level override sets LLMTimeoutMs + LLMIntent; the
	// resolver projects both into the typed Config fields.
	base := DefaultConfig()
	intent := "cheap"
	timeoutMs := 5000
	catalog := &stubCatalog{
		missions: map[string]*OverrideSpec{
			"mission-x": {
				LLMTimeoutMs: &timeoutMs,
				LLMIntent:    &intent,
			},
		},
	}
	e := NewExtensionWithConfig(slog.Default(), base, Deps{SkillCatalog: catalog})
	st := &resolveStateFake{id: "s", depth: 1, skill: "mission-x"}
	got := e.resolveTierConfig(context.Background(), st)
	if got.LLMTimeout != 5*time.Second {
		t.Errorf("LLMTimeout = %v, want 5s", got.LLMTimeout)
	}
	if got.LLMIntent != model.IntentCheap {
		t.Errorf("LLMIntent = %q, want cheap", got.LLMIntent)
	}
}

func TestResolveTierConfig_UnknownIntentInheritsFallback(t *testing.T) {
	// Per-skill overrides with unknown intent strings inherit
	// the prior-layer value silently (resolver-level discipline).
	base := DefaultConfig()
	base.LLMIntent = model.IntentSummarize
	bogus := "this-intent-does-not-exist"
	catalog := &stubCatalog{
		missions: map[string]*OverrideSpec{
			"mission-x": {LLMIntent: &bogus},
		},
	}
	e := NewExtensionWithConfig(slog.Default(), base, Deps{SkillCatalog: catalog})
	st := &resolveStateFake{id: "s", depth: 1, skill: "mission-x"}
	got := e.resolveTierConfig(context.Background(), st)
	if got.LLMIntent != model.IntentSummarize {
		t.Fatalf("LLMIntent = %q, want summarize (unknown override silently inherits)", got.LLMIntent)
	}
}

func TestResolveTierConfig_NilStateNoCrash(t *testing.T) {
	// Defensive: nil state should not panic — the resolver
	// returns the baseline Config unchanged.
	base := DefaultConfig()
	e := NewExtensionWithConfig(slog.Default(), base, Deps{})
	got := e.resolveTierConfig(context.Background(), nil)
	if got.MaxTurns != base.MaxTurns {
		t.Fatalf("nil-state resolve produced unexpected MaxTurns = %d, want %d", got.MaxTurns, base.MaxTurns)
	}
}

func TestApplyTierOverride_TimeoutAndRatio(t *testing.T) {
	// Direct unit on applyTierOverride — verify the duration +
	// ratio fields land and the absent fields stay put.
	cfg := DefaultConfig()
	cfg.LLMTimeout = 30 * time.Second
	cfg.TokenBudgetRatio = 0.0

	dur := 12 * time.Second
	applyTierOverride(&cfg, TierOverride{
		LLMTimeout:       ptrDuration(dur),
		TokenBudgetRatio: ptrFloat(0.7),
	})
	if cfg.LLMTimeout != dur {
		t.Errorf("LLMTimeout = %v, want %v", cfg.LLMTimeout, dur)
	}
	if cfg.TokenBudgetRatio != 0.7 {
		t.Errorf("TokenBudgetRatio = %v, want 0.7", cfg.TokenBudgetRatio)
	}
	// untouched fields stay at the prior value.
	if cfg.MaxTurns != DefaultConfig().MaxTurns {
		t.Errorf("MaxTurns mutated unexpectedly = %d", cfg.MaxTurns)
	}
}

func TestApplyOverrideSpec_L3CheckpointFields(t *testing.T) {
	// Direct unit on applyOverrideSpec — the L3 checkpoint/hide fields
	// (added so a skill / task / role can tune its own checkpoint+hide
	// aggressiveness) land, and absent fields stay put.
	cfg := DefaultConfig()
	cfg.CheckpointsEnabled = true
	cfg.CheckpointWindowTokens = 12000
	cfg.ContextHideRatio = 0.80

	applyOverrideSpec(&cfg, OverrideSpec{
		CheckpointWindowTokens: ptrInt(6000),
		ContextHideRatio:       ptrFloat(0.55),
	})
	if cfg.CheckpointWindowTokens != 6000 {
		t.Errorf("CheckpointWindowTokens = %d, want 6000", cfg.CheckpointWindowTokens)
	}
	if cfg.ContextHideRatio != 0.55 {
		t.Errorf("ContextHideRatio = %v, want 0.55", cfg.ContextHideRatio)
	}
	// CheckpointsEnabled was nil in the override → untouched.
	if !cfg.CheckpointsEnabled {
		t.Errorf("CheckpointsEnabled = false, want true (untouched)")
	}
}

func TestResolveTierConfig_L3CheckpointOverride_SkillBeatsTier(t *testing.T) {
	// End-to-end: a per-skill (task) override of the L3 checkpoint+hide
	// fields beats the per-tier value — the path build_task relies on to
	// run a tighter window than the shared worker tier.
	base := DefaultConfig()
	base.Tiers = map[string]TierOverride{
		"worker": {
			CheckpointWindowTokens: ptrInt(12000),
			ContextHideRatio:       ptrFloat(0.80),
		},
	}
	catalog := &stubCatalog{
		missions: map[string]*OverrideSpec{
			"build_task": {
				CheckpointWindowTokens: ptrInt(6000),
				ContextHideRatio:       ptrFloat(0.55),
			},
		},
	}
	e := NewExtensionWithConfig(slog.Default(), base, Deps{SkillCatalog: catalog})

	got := e.resolveTierConfig(context.Background(),
		&resolveStateFake{id: "b0", depth: 2, skill: "build_task", role: ""})
	if got.CheckpointWindowTokens != 6000 {
		t.Errorf("CheckpointWindowTokens = %d, want 6000 (skill override beats tier 12000)", got.CheckpointWindowTokens)
	}
	if got.ContextHideRatio != 0.55 {
		t.Errorf("ContextHideRatio = %v, want 0.55 (skill override beats tier 0.80)", got.ContextHideRatio)
	}
}
