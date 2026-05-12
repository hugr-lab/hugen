package session

import (
	"io/fs"
	"sync"
	"testing"

	"github.com/hugr-lab/hugen/assets"
	"github.com/hugr-lab/hugen/pkg/prompts"
)

// testPromptsOnce caches a single shared *prompts.Renderer across
// every test in the package. The bundled assets.PromptsFS is
// read-only at process scope so a single renderer is safe.
var (
	testPromptsOnce sync.Once
	testPromptsRdr  *prompts.Renderer
)

// testPrompts returns a *prompts.Renderer rooted at
// assets.PromptsFS — the production embedded bundle — with no
// operator override. Tests that exercise prompt rendering reach
// for it instead of constructing a renderer per case.
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
