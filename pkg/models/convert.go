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
					Arguments: normalizeToolArgs(tc.Args),
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

// normalizeToolArgs guarantees the wire format LLM providers
// expect for tool_use.input — Anthropic specifically rejects
// non-object inputs ("messages.N.content.M.tool_use.input: Input
// should be an object"). Nil / scalar / list values are coerced
// to an empty map; strings holding JSON object literals are
// decoded; anything else passes through.
//
// Background: when a model emits a tool call with no arguments,
// the parsed payload is `nil` (parseToolCalls leaves args
// untouched on empty Arguments). Re-sending nil to Anthropic
// surfaces as a 400 on the next round-trip.
func normalizeToolArgs(args any) any {
	switch v := args.(type) {
	case nil:
		return map[string]any{}
	case map[string]any:
		return v
	case string:
		if v == "" {
			return map[string]any{}
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(v), &decoded); err == nil {
			return decoded
		}
		// Not a JSON object — wrap so Anthropic still gets an
		// object; surface the raw string under a synthetic key.
		return map[string]any{"_raw": v}
	default:
		// Slices / scalars / structs: marshal-then-unmarshal-into-map
		// is the safest universal coercion. Failure → empty object.
		b, err := json.Marshal(v)
		if err != nil {
			return map[string]any{}
		}
		var decoded map[string]any
		if err := json.Unmarshal(b, &decoded); err == nil {
			return decoded
		}
		return map[string]any{"_raw": string(b)}
	}
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
