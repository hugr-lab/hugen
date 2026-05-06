package runtime

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/models"
)

// phaseModels runs phase 5: builds the LLM model router from
// cfg.Config.Models(). Populates Core.Models. The router is
// stateless w.r.t. cleanup — no resource closure needed.
func phaseModels(ctx context.Context, core *Core) error {
	modelService := models.New(
		ctx,
		core.LocalQuerier,
		core.RemoteQuerier,
		core.Config.Models(),
		models.WithLogger(core.Logger),
	)
	modelMap := models.BuildModelMap(modelService)
	modelDefaults := models.IntentDefaults(modelService)
	router, err := model.NewModelRouter(modelDefaults, modelMap)
	if err != nil {
		return err
	}
	core.Models = router
	core.Logger.Info("model router ready",
		"default", modelDefaults[model.IntentDefault].String(),
		"cheap", modelDefaults[model.IntentCheap].String())
	return nil
}
