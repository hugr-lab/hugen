package session

import "testing"

// TestIsContextTool pins the L3 block-exemption predicate: the four
// context:* checkpoint tools are exempt from the checkpoint_required /
// context_full dispatch blocks (the only way the model recovers),
// matched in both the canonical (":") and LLM-sanitized ("_") forms,
// while an unrelated provider tool named "context_*" is NOT exempt.
func TestIsContextTool(t *testing.T) {
	exempt := []string{
		"context:checkpoint", "context:hide", "context:expand", "context:rollback",
		"context_checkpoint", "context_hide", "context_expand", "context_rollback",
	}
	for _, n := range exempt {
		if !isContextTool(n) {
			t.Errorf("isContextTool(%q) = false, want true (must be block-exempt)", n)
		}
	}
	notExempt := []string{
		"bash-mcp:bash.read_file", "mission:finish", "context:reset",
		"context_other", "contextual:thing", "", "context", "context:",
	}
	for _, n := range notExempt {
		if isContextTool(n) {
			t.Errorf("isContextTool(%q) = true, want false", n)
		}
	}
}
