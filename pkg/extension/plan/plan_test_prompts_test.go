package plan

import (
	"io/fs"
	"sync"
	"testing"

	"github.com/hugr-lab/hugen/assets"
	"github.com/hugr-lab/hugen/pkg/prompts"
)

var (
	planPromptsOnce sync.Once
	planPromptsRdr  *prompts.Renderer
)

// planTestRenderer returns a *prompts.Renderer rooted at the
// production bundle so plan render tests exercise the live template
// rather than constructing a fixture per case.
func planTestRenderer(t *testing.T) *prompts.Renderer {
	t.Helper()
	planPromptsOnce.Do(func() {
		sub, err := fs.Sub(assets.PromptsFS, "prompts")
		if err != nil {
			t.Fatalf("fs.Sub: %v", err)
		}
		planPromptsRdr = prompts.NewRenderer(sub, "", nil)
	})
	return planPromptsRdr
}
