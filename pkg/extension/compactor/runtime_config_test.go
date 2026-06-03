package compactor

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/model"
	skillpkg "github.com/hugr-lab/hugen/pkg/skill"
)

func ptrString(s string) *string { return &s }

// TestBuildCompactorConfig_DefaultsApply verifies that an
// entirely empty operator-config block leaves the extension's
// DefaultConfig in place.
func TestBuildCompactorConfig_DefaultsApply(t *testing.T) {
	want := DefaultConfig()
	got := BuildConfig(config.CompactorConfig{}, slog.Default())
	if got.Enabled != want.Enabled ||
		got.MaxTurns != want.MaxTurns ||
		got.MaxTokens != want.MaxTokens ||
		got.PreservedRecentTurns != want.PreservedRecentTurns ||
		got.DigestMaxTokens != want.DigestMaxTokens ||
		got.MinTurnGap != want.MinTurnGap ||
		got.LLMTimeout != want.LLMTimeout ||
		got.LLMIntent != want.LLMIntent {
		t.Fatalf("absent operator config: got %+v, want defaults %+v", got, want)
	}
	if got.Tiers != nil {
		t.Errorf("Tiers should be nil when YAML carries no overlay; got %v", got.Tiers)
	}
}

// TestBuildCompactorConfig_UIMarkerEnabled verifies the
// `compactor.ui_marker.enabled` YAML toggle wires through to
// Config.UIMarkerEnabled. Phase 5.2 δ — global flag, no per-tier
// override.
func TestBuildCompactorConfig_UIMarkerEnabled(t *testing.T) {
	// Absent — defaults to true via DefaultConfig.
	def := BuildConfig(config.CompactorConfig{}, slog.Default())
	if !def.UIMarkerEnabled {
		t.Errorf("absent ui_marker block: UIMarkerEnabled = false, want true (default)")
	}
	// Explicit false — operator turned the marker off.
	off := BuildConfig(config.CompactorConfig{
		UIMarker: config.CompactorUIMarker{Enabled: ptrBool(false)},
	}, slog.Default())
	if off.UIMarkerEnabled {
		t.Errorf("explicit ui_marker.enabled=false: UIMarkerEnabled = true, want false")
	}
	// Explicit true — operator re-enabled (round-trip).
	on := BuildConfig(config.CompactorConfig{
		UIMarker: config.CompactorUIMarker{Enabled: ptrBool(true)},
	}, slog.Default())
	if !on.UIMarkerEnabled {
		t.Errorf("explicit ui_marker.enabled=true: UIMarkerEnabled = false, want true")
	}
}

// TestBuildCompactorConfig_CheckpointKnobs verifies the Stage 2 L3
// knobs wire from YAML through BuildConfig + the per-tier resolver:
// defaults when absent, top-level override, and a worker-tier overlay
// (the config.yaml dogfood shape — lower window + band on workers).
func TestBuildCompactorConfig_CheckpointKnobs(t *testing.T) {
	// Absent → DefaultConfig values.
	def := BuildConfig(config.CompactorConfig{}, slog.Default())
	if !def.CheckpointsEnabled || def.CheckpointWindowTokens != defaultCheckpointWindowTokens ||
		def.ContextHideRatio != defaultContextHideRatio {
		t.Fatalf("absent checkpoint config: got enabled=%v window=%d ratio=%v, want defaults",
			def.CheckpointsEnabled, def.CheckpointWindowTokens, def.ContextHideRatio)
	}

	// Top-level enabled + a worker-tier overlay lowering window + band.
	in := config.CompactorConfig{
		CheckpointsEnabled: ptrBool(true),
		Tiers: map[string]config.CompactorTier{
			"worker": {
				CheckpointWindowTokens: ptrInt(5000),
				ContextHideRatio:       ptrFloat(0.60),
			},
		},
	}
	built := BuildConfig(in, slog.Default())
	ext := NewExtensionWithConfig(slog.Default(), built, Deps{})

	st, _ := newSubagentState("ses-cfg", 1) // Tier() == "worker"
	cfg := ext.resolveTierConfig(context.Background(), st)
	if cfg.CheckpointWindowTokens != 5000 || cfg.ContextHideRatio != 0.60 {
		t.Fatalf("worker-tier resolved checkpoint config: window=%d ratio=%v, want 5000 / 0.60",
			cfg.CheckpointWindowTokens, cfg.ContextHideRatio)
	}
	if !cfg.CheckpointsEnabled {
		t.Fatalf("worker-tier CheckpointsEnabled = false, want true (inherited from top-level)")
	}
}

// TestBuildCompactorConfig_OperatorYAMLLaysOverDefaults verifies
// that explicit YAML values land on top of DefaultConfig.
func TestBuildCompactorConfig_OperatorYAMLLaysOverDefaults(t *testing.T) {
	in := config.CompactorConfig{
		Enabled:              ptrBool(false),
		MaxTurns:             999,
		MaxTokens:            123456,
		PreservedRecentTurns: 33,
		DigestMaxTokens:      7000,
		MinTurnGap:           7,
		LLMTimeoutMs:         5000,
		LLMIntent:            "cheap",
		TokenBudgetRatio:     0.55,
		Tiers: map[string]config.CompactorTier{
			"root": {
				Enabled:              ptrBool(true),
				PreservedRecentTurns: ptrInt(20),
				TokenBudgetRatio:     ptrFloat(0.7),
			},
		},
	}
	got := BuildConfig(in, slog.Default())
	if got.Enabled {
		t.Errorf("Enabled = true, want false (explicit operator override)")
	}
	if got.MaxTurns != 999 {
		t.Errorf("MaxTurns = %d, want 999", got.MaxTurns)
	}
	if got.LLMTimeout != 5*time.Second {
		t.Errorf("LLMTimeout = %v, want 5s", got.LLMTimeout)
	}
	if got.LLMIntent != model.IntentCheap {
		t.Errorf("LLMIntent = %q, want cheap", got.LLMIntent)
	}
	if got.TokenBudgetRatio != 0.55 {
		t.Errorf("TokenBudgetRatio = %v, want 0.55", got.TokenBudgetRatio)
	}
	if len(got.Tiers) != 1 {
		t.Fatalf("Tiers len = %d, want 1", len(got.Tiers))
	}
	root := got.Tiers["root"]
	if root.Enabled == nil || !*root.Enabled {
		t.Errorf("Tiers[root].Enabled = %v, want &true", root.Enabled)
	}
	if root.PreservedRecentTurns == nil || *root.PreservedRecentTurns != 20 {
		t.Errorf("Tiers[root].PreservedRecentTurns = %v, want &20", root.PreservedRecentTurns)
	}
	if root.TokenBudgetRatio == nil || *root.TokenBudgetRatio != 0.7 {
		t.Errorf("Tiers[root].TokenBudgetRatio = %v, want &0.7", root.TokenBudgetRatio)
	}
}

// TestBuildCompactorConfig_UnknownIntentWarnsAndFallsBack
// verifies the operator-supplied intent string discipline: an
// unknown value falls back to IntentSummarize and emits a warn
// log so the operator sees the misconfiguration at boot.
func TestBuildCompactorConfig_UnknownIntentWarnsAndFallsBack(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	in := config.CompactorConfig{LLMIntent: "yolo-mode"}
	got := BuildConfig(in, logger)
	if got.LLMIntent != model.IntentSummarize {
		t.Errorf("LLMIntent = %q, want summarize (fallback)", got.LLMIntent)
	}
	if !strings.Contains(buf.String(), "unknown llm_intent") {
		t.Errorf("log output missing warn: %q", buf.String())
	}
}

// TestProjectTierOverride_AbsentFieldsStayNil verifies the
// per-tier pointer-shape: only explicitly-set fields in the
// YAML side become non-nil after projection.
func TestProjectTierOverride_AbsentFieldsStayNil(t *testing.T) {
	in := config.CompactorTier{
		Enabled:  ptrBool(false),
		MaxTurns: ptrInt(40),
		// other fields left nil — must stay nil after projection.
	}
	out := projectTierOverride(in, slog.Default())
	if out.Enabled == nil || *out.Enabled {
		t.Errorf("Enabled = %v, want &false", out.Enabled)
	}
	if out.MaxTurns == nil || *out.MaxTurns != 40 {
		t.Errorf("MaxTurns = %v, want &40", out.MaxTurns)
	}
	if out.LLMIntent != nil {
		t.Errorf("LLMIntent should stay nil; got %v", *out.LLMIntent)
	}
	if out.LLMTimeout != nil {
		t.Errorf("LLMTimeout should stay nil; got %v", *out.LLMTimeout)
	}
	if out.TokenBudgetRatio != nil {
		t.Errorf("TokenBudgetRatio should stay nil; got %v", *out.TokenBudgetRatio)
	}
}

// TestProjectCompactorOverride_RoundTrip verifies the
// skill-manifest projection mirrors every pointer field
// verbatim onto the extension's wire shape.
func TestProjectCompactorOverride_RoundTrip(t *testing.T) {
	in := &skillpkg.CompactorOverride{
		Enabled:              ptrBool(true),
		MaxTurns:             ptrInt(80),
		PreservedRecentTurns: ptrInt(15),
		LLMTimeoutMs:         ptrInt(20000),
		LLMIntent:            ptrString("cheap"),
		TokenBudgetRatio:     ptrFloat(0.5),
	}
	got := projectCompactorOverride(in)
	if got == nil {
		t.Fatalf("projection returned nil for populated input")
	}
	if got.Enabled == nil || !*got.Enabled {
		t.Errorf("Enabled = %v, want &true", got.Enabled)
	}
	if got.MaxTurns == nil || *got.MaxTurns != 80 {
		t.Errorf("MaxTurns = %v, want &80", got.MaxTurns)
	}
	if got.LLMTimeoutMs == nil || *got.LLMTimeoutMs != 20000 {
		t.Errorf("LLMTimeoutMs = %v, want &20000", got.LLMTimeoutMs)
	}
	if got.LLMIntent == nil || *got.LLMIntent != "cheap" {
		t.Errorf("LLMIntent = %v, want &cheap", got.LLMIntent)
	}
}

func TestProjectCompactorOverride_NilStaysNil(t *testing.T) {
	if got := projectCompactorOverride(nil); got != nil {
		t.Fatalf("projectCompactorOverride(nil) = %+v, want nil", got)
	}
}

// TestSkillManagerCompactorCatalog_NilSafe verifies the adapter
// stays safe when the SkillManager handle is nil — production
// wiring is non-nil, but a defensive contract keeps test
// fixtures simple.
func TestSkillManagerCompactorCatalog_NilSafe(t *testing.T) {
	cat := NewSkillManagerCatalog(nil)
	mission, role, err := cat.LookupCompactor(context.Background(), "anything", "anything")
	if err != nil {
		t.Fatalf("LookupCompactor returned err = %v, want nil", err)
	}
	if mission != nil || role != nil {
		t.Errorf("nil manager should return (nil, nil); got mission=%+v role=%+v", mission, role)
	}
}
