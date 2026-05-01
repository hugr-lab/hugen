// Conversion between pkg/model types and Hugr's chat_completion
// wire format. Phase 2 (R-Plan-23) replaced the ADK genai-shaped
// conversions with native model.Message → JSON and model.Tool → JSON.
package models

import (
	"encoding/json"
	"fmt"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/model"
)

// messagesToHugrJSON converts pkg/model.Message values into the JSON
// strings the Hugr chat_completion subscription expects in its
// `messages` argument.
func messagesToHugrJSON(messages []model.Message) ([]string, error) {
	out := make([]string, 0, len(messages))
	for i, msg := range messages {
		role := mapRole(msg.Role)
		if role == "" {
			return nil, fmt.Errorf("messagesToHugrJSON: empty role on message %d", i)
		}
		hm := types.LLMMessage{
			Role:             role,
			Content:          msg.Content,
			ToolCallID:       msg.ToolCallID,
			Thinking:         msg.Thinking,
			ThoughtSignature: msg.ThoughtSignature,
		}
		if len(msg.ToolCalls) > 0 {
			calls := make([]types.LLMToolCall, len(msg.ToolCalls))
			for i, tc := range msg.ToolCalls {
				calls[i] = types.LLMToolCall{
					ID:        tc.ID,
					Name:      tc.Name,
					Arguments: tc.Args,
				}
			}
			hm.ToolCalls = calls
		}
		b, err := json.Marshal(hm)
		if err != nil {
			return nil, fmt.Errorf("messagesToHugrJSON: marshal %d: %w", i, err)
		}
		out = append(out, string(b))
	}
	return out, nil
}

// mapRole maps pkg/model role names onto the role names Hugr
// expects (which mirror the OpenAI chat-completion role vocabulary).
func mapRole(role string) string {
	switch role {
	case model.RoleUser:
		return "user"
	case model.RoleAssistant, "model":
		return "assistant"
	case model.RoleSystem:
		return "system"
	case model.RoleTool, "function":
		return "tool"
	}
	return ""
}

// toolsToHugrJSON encodes pkg/model.Tool declarations as the JSON
// strings expected by the Hugr chat_completion `tools` argument.
//
// Phase 1 / 2 don't populate Tools (phase 3 ships skill tools), so
// this function exists for forward-compatibility only.
func toolsToHugrJSON(tools []model.Tool) ([]string, error) {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		schema := t.Schema
		if schema == nil {
			schema = map[string]any{
				"type":       "object",
				"properties": map[string]any{},
				"required":   []string{},
			}
		}
		payload := types.LLMTool{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  schema,
		}
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("toolsToHugrJSON: marshal %q: %w", t.Name, err)
		}
		out = append(out, string(b))
	}
	return out, nil
}
