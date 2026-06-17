package task

import (
	"encoding/json"
	"testing"

	"github.com/hugr-lab/hugen/pkg/tool"
)

// TestStaticTaskToolSchemasConform guards that the task provider's own
// static tool schemas conform to the conservative JSON-Schema subset
// every chat-completion provider accepts (Anthropic / OpenAI / Gemini).
// A stray `additionalProperties` (etc.) 400s Gemini's whole tools
// payload — these are OUR schemas, so they must be clean at the source,
// not only sanitized at the ToolManager net.
func TestStaticTaskToolSchemasConform(t *testing.T) {
	for _, tl := range []tool.Tool{executeTaskTool(), searchTaskTool(), describeTaskTool()} {
		if err := tool.ValidateLLMSchema(tl.ArgSchema); err != nil {
			t.Errorf("%s arg schema not LLM-conformant: %v\nschema=%s", tl.Name, err, tl.ArgSchema)
		}
	}
	// The no-inputs synthetic task:<name> default (List's else branch).
	if err := tool.ValidateLLMSchema(json.RawMessage(`{"type":"object","properties":{}}`)); err != nil {
		t.Errorf("no-input task:<name> default schema not conformant: %v", err)
	}
}
