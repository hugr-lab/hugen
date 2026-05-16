//go:build duckdb_arrow && scenario

package harness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oasdiff/yaml"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/auth/sources/oidc"
	"github.com/hugr-lab/hugen/pkg/runtime"
)

// Runtime is the harness's view of a booted *runtime.Core.
// One Runtime per Run; sessions live inside it.
type Runtime struct {
	Core      *runtime.Core
	Run       *Run
	Env       map[string]string
	RunDir    string
	AgentCfg  string
	// runsRoot is tests/scenarios/, used by InstallFixtures to
	// resolve fixture paths.
	runsRoot string
	// buildCfg captures the parameters used to construct Core, so
	// Restart() can rebuild a fresh Core against the same StateDir
	// after a graceful Shutdown. Phase 5.2 ι (restore_parked_replay).
	buildCfg  runtime.Config
	logger    *slog.Logger
}

// Setup loads .test.env, evaluates the Run's requires:, merges the
// LLM + topology overlays onto the prod config.yaml, projects a
// runtime.Config, calls runtime.Build, and returns the Runtime.
//
// Calls t.Skip(reason) when the run's environment is missing — the
// harness deliberately does not crash on absent credentials so
// `make scenario` walks every run cleanly even on a bare developer
// machine.
//
// t.Cleanup wires Core.Shutdown so a panic mid-test still drains
// the engine.
func Setup(ctx context.Context, t *testing.T, opts SetupOpts) *Runtime {
	t.Helper()
	require := func(cond bool, format string, args ...any) {
		if !cond {
			t.Fatalf(format, args...)
		}
	}
	require(opts.Run != nil, "harness.Setup: Run is required")
	require(opts.RunsPath != "", "harness.Setup: RunsPath is required")

	env, err := LoadDotEnv(opts.EnvFile)
	if err != nil {
		t.Fatalf("harness.Setup: load env: %v", err)
	}

	// Inject repo-relative paths the topology configs reference via
	// ${HUGEN_BIN_DIR} / ${HUGEN_VENDOR_DIR}. Test cwd is
	// tests/scenarios/, so unqualified `./bin/...` would resolve to
	// tests/scenarios/bin/ which doesn't exist. We pre-compute
	// absolute paths from repo root and let mergeConfigs / runtime
	// expand them.
	if env["HUGEN_BIN_DIR"] == "" {
		if abs, err := filepath.Abs(filepath.Join(opts.RunsRoot, "..", "..", "bin")); err == nil {
			env["HUGEN_BIN_DIR"] = abs
		}
	}
	if env["HUGEN_VENDOR_DIR"] == "" {
		if abs, err := filepath.Abs(filepath.Join(opts.RunsRoot, "..", "..", "vendor")); err == nil {
			env["HUGEN_VENDOR_DIR"] = abs
		}
	}

	if ok, reason := EvalRequires(env, opts.Run.Requires); !ok {
		t.Skipf("scenario run %q skipped: %s", opts.Run.Name, reason)
	}

	// Apply env to os.Environ for the duration of the run so config
	// loaders that resolve ${VAR} placeholders see the harness's
	// values. Save originals to restore in t.Cleanup.
	for k, v := range env {
		if old, had := os.LookupEnv(k); had {
			t.Cleanup(func() { _ = os.Setenv(k, old) })
		} else {
			t.Cleanup(func() { _ = os.Unsetenv(k) })
		}
		_ = os.Setenv(k, v)
	}

	runDir := opts.RunDir
	if runDir == "" {
		runDir = filepath.Join(opts.RunsRoot, ".data",
			fmt.Sprintf("run-%s", time.Now().UTC().Format("20060102-150405")),
			opts.Run.Name)
	}
	dirs := []string{
		runDir,
		filepath.Join(runDir, "state"),
		filepath.Join(runDir, "state", "skills", "system"),
		filepath.Join(runDir, "workspaces"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("harness.Setup: mkdir %s: %v", d, err)
		}
	}

	// Merge: prod config.yaml ← LLM overlay ← topology overlay.
	// Output written into runDir so the run's exact agent-config is
	// preserved on disk for human review.
	agentCfgPath := filepath.Join(runDir, "agent-config.yaml")
	if err := mergeConfigs(opts.BaseConfigPath,
		ResolveRunPath(opts.RunsPath, opts.Run.LLM),
		ResolveRunPath(opts.RunsPath, opts.Run.Topology),
		agentCfgPath); err != nil {
		t.Fatalf("harness.Setup: merge configs: %v", err)
	}

	// HTTP port: Keycloak whitelists redirect_uri by port, so we
	// can't pick a free one — must match an entry the OIDC client
	// is registered with. Default 10000 (HUGEN_PORT in prod .env);
	// override via HUGEN_PORT in .test.env when the deployment
	// reserves a separate AGENT_PORT for harness use.
	port := 10000
	if v := env["HUGEN_PORT"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			port = n
		}
	}

	logLevel := slog.LevelInfo
	if env["HUGEN_LOG_LEVEL"] == "debug" || os.Getenv("HUGEN_LOG_LEVEL") == "debug" {
		logLevel = slog.LevelDebug
	}
	logFile, err := os.Create(filepath.Join(runDir, "runtime.log"))
	if err != nil {
		t.Fatalf("harness.Setup: open runtime.log: %v", err)
	}
	t.Cleanup(func() { _ = logFile.Close() })
	logger := slog.New(slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: logLevel}))

	cfg := runtime.Config{
		Logger:          logger,
		Mode:            "local", // harness always runs as autonomous-agent
		AgentConfigPath: agentCfgPath,
		StateDir:        filepath.Join(runDir, "state"),
		Workspace: runtime.WorkspaceConfig{
			Dir:            filepath.Join(runDir, "workspaces"),
			CleanupOnClose: false,
		},
		HTTP: runtime.HTTPConfig{
			Port:    port,
			BaseURI: fmt.Sprintf("http://localhost:%d", port),
		},
		Hugr: runtime.HugrConfig{
			URL:     env["HUGR_URL"],
			Timeout: 60 * time.Second,
		},
		AfterAuthHook: makeTokenInjectionHook(env, logger),
	}

	core, err := runtime.Build(ctx, cfg)
	if err != nil {
		t.Fatalf("harness.Setup: runtime.Build: %v", err)
	}

	rt := &Runtime{
		Core:     core,
		Run:      opts.Run,
		Env:      env,
		RunDir:   runDir,
		AgentCfg: agentCfgPath,
		runsRoot: opts.RunsRoot,
		buildCfg: cfg,
		logger:   logger,
	}
	// Shutdown is wired on the live Core pointer rather than the
	// captured closure variable so a Runtime.Restart that swaps
	// rt.Core mid-test still cleans up whichever Core is alive at
	// teardown time.
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if rt.Core != nil {
			rt.Core.Shutdown(shutCtx)
		}
	})

	logger.Info("harness setup complete",
		"run", opts.Run.Name, "run_dir", runDir,
		"agent_config", agentCfgPath, "http_port", port)

	return rt
}

// InstallFixtures copies test-only skill bundles from
// `tests/scenarios/fixtures/<name>/` into the run's local skill
// backend, then refreshes the SkillStore's List cache so the new
// entries surface on the next skill discovery call. Called by the
// runner before scenarios that declare `fixtures:` in their YAML.
// Phase 5.2 ι.
//
// Idempotent: a fixture already present on disk is overwritten;
// the SkillStore refresh is best-effort. Failures fatal — the
// scenario depends on the fixture being there.
func (r *Runtime) InstallFixtures(t *testing.T, names []string) {
	t.Helper()
	if len(names) == 0 {
		return
	}
	localRoot := filepath.Join(r.buildCfg.StateDir, "skills", "local")
	if err := os.MkdirAll(localRoot, 0o755); err != nil {
		t.Fatalf("InstallFixtures: mkdir local: %v", err)
	}
	for _, name := range names {
		src := filepath.Join(r.runsRoot, "fixtures", name)
		dst := filepath.Join(localRoot, name)
		if err := copyTree(src, dst); err != nil {
			t.Fatalf("InstallFixtures: copy %s → %s: %v", src, dst, err)
		}
	}
	if r.Core != nil && r.Core.SkillStore != nil {
		// The concrete *skill.Store implements Refresh; the
		// SkillStore interface deliberately omits it (operators
		// don't refresh). Type-assert and skip cleanly if some
		// future test wires a different backend.
		if refresher, ok := r.Core.SkillStore.(interface{ Refresh() }); ok {
			refresher.Refresh()
		}
	}
}

// Restart gracefully stops the current Core, rebuilds a fresh
// Core against the same StateDir, and runs Manager.RestoreActive
// so non-terminal roots reattach. After Restart returns, callers
// holding a SessionHandle from before must reattach via
// SessionHandle.ReattachAfterRestart to refresh the session
// pointer. Phase 5.2 ι (restore_parked_replay).
func (r *Runtime) Restart(ctx context.Context, t *testing.T) {
	t.Helper()
	if r.Core == nil {
		t.Fatalf("Restart: Core is nil")
	}
	prev := r.Core
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	prev.Shutdown(shutCtx)
	cancel()

	core, err := runtime.Build(ctx, r.buildCfg)
	if err != nil {
		t.Fatalf("Restart: runtime.Build: %v", err)
	}
	if err := core.Manager.RestoreActive(ctx); err != nil {
		t.Fatalf("Restart: RestoreActive: %v", err)
	}
	r.Core = core
	r.logger.Info("harness restart complete",
		"run", r.Run.Name, "run_dir", r.RunDir)
}

// copyTree mirrors src directory into dst, creating dst when
// absent. Files are copied byte-for-byte with the source's mode.
// Skips dotfiles and symlinks to keep the contract simple — the
// fixture set is small and curated.
func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat src: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("fixture %s is not a directory", src)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("mkdir dst: %w", err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("readdir: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		sp := filepath.Join(src, name)
		dp := filepath.Join(dst, name)
		if e.IsDir() {
			if err := copyTree(sp, dp); err != nil {
				return err
			}
			continue
		}
		data, err := os.ReadFile(sp)
		if err != nil {
			return fmt.Errorf("read %s: %w", sp, err)
		}
		einfo, err := e.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", sp, err)
		}
		if err := os.WriteFile(dp, data, einfo.Mode().Perm()); err != nil {
			return fmt.Errorf("write %s: %w", dp, err)
		}
	}
	return nil
}

// SetupOpts bundles Setup's required + optional inputs.
type SetupOpts struct {
	// Run is the run.yaml entry to boot. Required.
	Run *Run
	// RunsPath is the absolute path to the runs.yaml the Run came
	// from — used to resolve relative LLM / topology paths.
	RunsPath string
	// RunsRoot is tests/scenarios/ — used to derive the .data/ path
	// when RunDir is empty.
	RunsRoot string
	// EnvFile is the .test.env path. Required.
	EnvFile string
	// BaseConfigPath is the prod config.yaml at repo root. Every
	// agent-config inherits from this.
	BaseConfigPath string
	// RunDir overrides the default .data/run-<ts>/<name>/ path.
	// Empty = compute via RunsRoot + timestamp.
	RunDir string
}

// makeTokenInjectionHook returns the AfterAuthHook that injects
// pre-captured tokens into the hugr OIDC source. Returns nil when
// no HUGR_ACCESS_TOKEN is set (harness can still run scenarios
// that don't need Hugr).
func makeTokenInjectionHook(env map[string]string, logger *slog.Logger) func(context.Context, *auth.Service) error {
	access := env["HUGR_ACCESS_TOKEN"]
	if access == "" {
		return nil
	}
	refresh := env["HUGR_REFRESH_TOKEN"]
	expiresAt := time.Now().Add(15 * time.Minute) // conservative default
	if v := env["HUGR_TOKEN_EXPIRES_AT"]; v != "" {
		if parsed, err := time.Parse(time.RFC3339, v); err == nil {
			expiresAt = parsed
		}
	}
	return func(_ context.Context, svc *auth.Service) error {
		ts, ok := svc.TokenStore("hugr")
		if !ok {
			logger.Warn("AfterAuthHook: hugr token store not registered; skipping injection")
			return nil
		}
		oidcSrc, ok := ts.(*oidc.Source)
		if !ok {
			// Static-token mode (RemoteStore) — already configured
			// from runtime.Config.Hugr.AccessToken; nothing to do.
			return nil
		}
		oidcSrc.SetTokens(access, refresh, expiresAt)
		logger.Info("AfterAuthHook: injected captured OIDC tokens",
			"expires_at", expiresAt)
		return nil
	}
}

// mergeConfigs deep-merges base ← llmOverlay ← topologyOverlay
// into out. Maps merge recursively; lists are replaced (overlay
// wins). Scalars are replaced (overlay wins).
func mergeConfigs(base, llmOverlay, topologyOverlay, out string) error {
	merged, err := readYAMLAsMap(base)
	if err != nil {
		return fmt.Errorf("read base %s: %w", base, err)
	}
	for _, overlay := range []string{llmOverlay, topologyOverlay} {
		if overlay == "" {
			continue
		}
		ov, err := readYAMLAsMap(overlay)
		if err != nil {
			return fmt.Errorf("read overlay %s: %w", overlay, err)
		}
		merged = deepMerge(merged, ov)
	}
	body, err := yaml.Marshal(merged)
	if err != nil {
		return fmt.Errorf("marshal merged: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	return os.WriteFile(out, body, 0o600)
}

func readYAMLAsMap(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// deepMerge: keys present in overlay overwrite base; nested maps
// merge recursively; lists/scalars are replaced wholesale.
// Returns a new map; inputs are not mutated.
func deepMerge(base, overlay map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(overlay))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overlay {
		if existing, ok := out[k]; ok {
			if em, ok := existing.(map[string]any); ok {
				if om, ok := v.(map[string]any); ok {
					out[k] = deepMerge(em, om)
					continue
				}
			}
		}
		out[k] = v
	}
	return out
}

// SortedKeys returns the keys of m in a deterministic order.
// Test helper used by harness self-tests; exported for use by
// external assertion code.
func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// errUnused keeps the errors import live in case future helpers
// want it without churning the import block.
var _ = errors.New
