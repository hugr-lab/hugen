package runtime

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/store/local"

	hugr "github.com/hugr-lab/query-engine"
	"github.com/hugr-lab/query-engine/types"
)

// BuildConfigService loads the YAML config aggregate (carried via
// identity.Agent.Config) and returns the phase-3 *config.StaticService.
// Every domain consumer reads through narrow Views off this Service —
// no other config aggregate lives in pkg/runtime. The runtime agent
// identity stays a separate live dependency (identity.Source) — it
// is not snapshotted here.
func BuildConfigService(ctx context.Context, src identity.Source, isLocal bool) (*config.StaticService, error) {
	agent, err := src.Agent(ctx)
	if err != nil {
		return nil, err
	}
	in, err := config.LoadStaticInput(agent.Config, isLocal)
	if err != nil {
		return nil, err
	}
	if in.Models.Model == "" {
		return nil, fmt.Errorf("config: models.model is empty (set in config.yaml)")
	}
	return config.NewStaticService(in), nil
}

// BuildLocalEngine opens the embedded DuckDB engine for local mode.
// Returns the typed *hugr.Service handle so Shutdown can call its
// Close method without an io.Closer round-trip.
func BuildLocalEngine(
	ctx context.Context,
	localView config.LocalView,
	embedView config.EmbeddingView,
	idSrc identity.Source,
	logger *slog.Logger,
) (*hugr.Service, error) {
	return local.New(ctx, localView, embedView, idSrc, logger)
}

// ChooseStore picks the querier the runtime store talks to. The
// agent runs in exactly one of two modes:
//
//   - local mode (LocalDBEnabled): localQ is the embedded DuckDB.
//     All sessions, events, notes, and memory live inside the
//     agent process.
//   - remote mode: remoteQ is the upstream hugr GraphQL endpoint;
//     localQ is nil. Sessions / memory / artifacts persist in the
//     shared hub DB and the agent identifies itself by the bearer
//     token its identity source supplies. The schema is the same —
//     session.NewRuntimeStoreLocal is mode-agnostic; the "local"
//     in its name refers to the Go-side facade, not the DB.
//
// Mixing the two queriers would split state across stores and is
// not supported.
func ChooseStore(localQ, remoteQ types.Querier, embedderEnabled bool) session.RuntimeStore {
	if localQ != nil {
		return session.NewRuntimeStoreLocal(localQ, embedderEnabled)
	}
	if remoteQ != nil {
		return session.NewRuntimeStoreLocal(remoteQ, embedderEnabled)
	}
	return nil
}

// phaseStorage runs phase 4: loads the config aggregate, opens the
// local engine if enabled, and selects the runtime store. Populates
// Core.Config, Core.LocalEngine, Core.LocalQuerier, Core.Store.
// Also re-applies auth sources from the loaded config view onto
// Core.Auth (mutates phase-2 service in place).
//
// Note: spec §9 step 14 also calls for constructing the
// policies-store + policies.New singleton here. That part is
// deferred until M1 (native policies impl + DuckDB Store backend)
// closes — the wrapper-shape policies in pkg/tool/providers/policies
// still requires a perm.Service which only phase 7 produces. Once
// the native impl accepts a nil-perms-set-later wiring, Core.Policies
// will land here.
func phaseStorage(ctx context.Context, core *Core) error {
	cfgSvc, err := BuildConfigService(ctx, core.Identity, core.Cfg.Mode == "local")
	if err != nil {
		return err
	}
	core.Config = cfgSvc

	if err := core.Auth.LoadFromView(ctx, cfgSvc.Auth()); err != nil {
		return fmt.Errorf("auth sources: %w", err)
	}

	localView := cfgSvc.Local()
	embedView := cfgSvc.Embedding()

	if localView.LocalDBEnabled() {
		eng, err := BuildLocalEngine(ctx, localView, embedView, core.Identity, core.Logger)
		if err != nil {
			return fmt.Errorf("local engine: %w", err)
		}
		core.LocalEngine = eng
		core.LocalQuerier = eng
		core.addCleanup(func() {
			if err := eng.Close(); err != nil {
				core.Logger.Warn("cleanup: close local engine", "err", err)
			}
		})
	}

	embed := embedView.EmbeddingConfig()
	embedderEnabled := embed.Mode != "" && embed.Model != ""
	store := ChooseStore(core.LocalQuerier, core.RemoteQuerier, embedderEnabled)
	if store == nil {
		return fmt.Errorf("no querier available (need local engine or remote hub)")
	}
	core.Store = store
	return nil
}
