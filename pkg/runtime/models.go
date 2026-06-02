package runtime

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/models"
)

// phaseModels runs phase 5: builds the LLM model router from
// cfg.Config.Models(). Populates Core.Models. The router is
// stateless w.r.t. cleanup — no resource closure needed.
func phaseModels(ctx context.Context, core *Core) error {
	mc := core.Config.Models()
	modelService := models.New(
		ctx,
		core.LocalQuerier,
		core.RemoteQuerier,
		mc,
		models.WithLogger(core.Logger),
	)
	modelMap := models.BuildModelMap(modelService)
	modelDefaults := models.IntentDefaults(modelService)
	router, err := model.NewModelRouter(modelDefaults, modelMap)
	if err != nil {
		return err
	}
	budgets, defaultBudget, ratios, intentBudgets := contextBudgetsFromConfig(mc.ModelsConfig(), modelMap, modelDefaults)
	router.SetContextBudgets(budgets, defaultBudget, ratios, intentBudgets)
	core.Models = router
	core.Logger.Info("model router ready",
		"default", modelDefaults[model.IntentDefault].String(),
		"cheap", modelDefaults[model.IntentCheap].String())
	return nil
}

// contextBudgetsFromConfig projects the operator `models:` config onto
// the per-spec context windows + per-intent soft ratios the router's
// budget accessors consume (Phase 5.2 budget-termination). Budgets are
// keyed by the registered ModelSpec, looked up from
// ContextWindows[<model name>]; ratios fall back to the top-level
// context_budget_ratio when a route omits one.
func contextBudgetsFromConfig(mc config.ModelsConfig, modelMap map[model.ModelSpec]model.Model, defaults map[model.Intent]model.ModelSpec) (map[model.ModelSpec]int, int, map[model.Intent]float64, map[model.Intent]int) {
	budgets := make(map[model.ModelSpec]int, len(modelMap))
	for spec := range modelMap {
		if w, ok := mc.ContextWindows[spec.Name]; ok && w > 0 {
			budgets[spec] = w
		}
	}
	ratios := make(map[model.Intent]float64, len(defaults))
	intentBudgets := make(map[model.Intent]int, len(defaults))
	for intent := range defaults {
		rt, hasRoute := mc.Routes[string(intent)]
		if hasRoute && rt.ContextBudgetRatio > 0 {
			ratios[intent] = rt.ContextBudgetRatio
		} else if mc.ContextBudgetRatio > 0 {
			ratios[intent] = mc.ContextBudgetRatio
		}
		// Per-intent budget override — routes.<intent>.default_budget
		// lets a dedicated worker intent run a tighter budget than the
		// model it shares with the orchestration roles.
		if hasRoute && rt.DefaultBudget > 0 {
			intentBudgets[intent] = rt.DefaultBudget
		}
	}
	return budgets, mc.DefaultBudget, ratios, intentBudgets
}
