package manager

import (
	"io/fs"
	"sync"
	"testing"

	"github.com/hugr-lab/hugen/assets"
	"github.com/hugr-lab/hugen/pkg/prompts"
)

// testPromptsOnce caches a single shared *prompts.Renderer across
// every test in the package. assets.PromptsFS is process-scope
// read-only so one renderer is safe.
var (
	testPromptsOnce sync.Once
	testPromptsRdr  *prompts.Renderer
)

// testPrompts returns a *prompts.Renderer rooted at the production
// bundle. Manager tests pass this via WithPrompts so the soft-
// warning / spawned-note / subagent-result render paths reach a
// live renderer instead of a nil one.
func testPrompts(t *testing.T) *prompts.Renderer {
	t.Helper()
	testPromptsOnce.Do(func() {
		sub, err := fs.Sub(assets.PromptsFS, "prompts")
		if err != nil {
			t.Fatalf("fs.Sub prompts: %v", err)
		}
		testPromptsRdr = prompts.NewRenderer(sub, "", nil)
	})
	return testPromptsRdr
}
