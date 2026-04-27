// Package hugrmodel implements ADK model.LLM interface using Hugr GraphQL.
package models

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hugr-lab/query-engine/types"

	"google.golang.org/genai"
)

// adkToHugrMessages converts ADK Content messages to JSON-encoded strings
// for the Hugr GraphQL chat_completion messages parameter.
func adkToHugrMessages(contents []*genai.Content) ([]string, error) {
	var messages []string
	for _, c := range contents {
		if c == nil {
			continue
		}
		msgs, err := contentToHugrMessages(c)
		if err != nil {
			return nil, fmt.Errorf("convert content (role=%s): %w", c.Role, err)
		}
		messages = append(messages, msgs...)
	}
	return messages, nil
}

func contentToHugrMessages(c *genai.Content) ([]string, error) {
	role := mapRole(c.Role)
	var result []string

	// Collect all function calls and text parts separately.
	// OpenAI requires all tool_calls from one turn in a single assistant message.
	var textParts []string
	var toolCalls []types.LLMToolCall
	var toolResponses []types.LLMMessage

	// Gemini 2.5+: capture ThoughtSignature from the first functionCall Part.
	// ADK stores it on genai.Part; Hugr expects it on LLMMessage level.
	var thoughtSig string

	for _, p := range c.Parts {
		switch {
		case p.FunctionCall != nil:
			args := p.FunctionCall.Args
			if args == nil {
				args = map[string]any{}
			}
			toolCalls = append(toolCalls, types.LLMToolCall{
				ID:        p.FunctionCall.ID,
				Name:      p.FunctionCall.Name,
				Arguments: args,
			})
			if thoughtSig == "" && len(p.ThoughtSignature) > 0 {
				thoughtSig = string(p.ThoughtSignature)
			}

		case p.FunctionResponse != nil:
			toolResponses = append(toolResponses, types.LLMMessage{
				Role:       "tool",
				Content:    formatFunctionResponse(p.FunctionResponse.Response),
				ToolCallID: p.FunctionResponse.ID,
			})

		case p.Thought:
			// Skip thinking content — Hugr LLMMessage has no thought field.
			// ThoughtSignature on the first functionCall Part provides continuity.

		case p.Text != "":
			textParts = append(textParts, p.Text)
		}
	}

	// Emit text + tool_calls as a single assistant message (OpenAI requires
	// all tool_calls from one turn in one message).
	if len(toolCalls) > 0 {
		text := strings.Join(textParts, "")
		msg := types.LLMMessage{
			Role:             "assistant",
			Content:          text,
			ToolCalls:        toolCalls,
			ThoughtSignature: thoughtSig,
		}
		b, err := json.Marshal(msg)
		if err != nil {
			return nil, fmt.Errorf("marshal assistant message: %w", err)
		}
		result = append(result, string(b))
	} else if len(textParts) > 0 {
		msg := types.LLMMessage{
			Role:    role,
			Content: strings.Join(textParts, ""),
		}
		b, err := json.Marshal(msg)
		if err != nil {
			return nil, fmt.Errorf("marshal text message: %w", err)
		}
		result = append(result, string(b))
	}

	// Emit each tool response as a separate message.
	for _, tr := range toolResponses {
		b, err := json.Marshal(tr)
		if err != nil {
			return nil, fmt.Errorf("marshal tool response: %w", err)
		}
		result = append(result, string(b))
	}

	// If content had parts but none matched, emit an empty message.
	if len(result) == 0 && len(c.Parts) > 0 {
		msg := types.LLMMessage{Role: role, Content: ""}
		b, _ := json.Marshal(msg)
		result = append(result, string(b))
	}

	return result, nil
}

func mapRole(role string) string {
	switch role {
	case "model":
		return "assistant"
	case "function":
		return "tool"
	default:
		return role
	}
}

func formatFunctionResponse(resp map[string]any) string {
	if resp == nil {
		return "{}"
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return fmt.Sprintf("%v", resp)
	}
	return string(b)
}

// adkToHugrTools converts ADK genai.Tool FunctionDeclarations to JSON-encoded
// strings for the Hugr GraphQL chat_completion tools parameter.
// ADK stores tool declarations in req.Config.Tools (not req.Tools).
//
// ADK mcptoolset puts MCP InputSchema into ParametersJsonSchema (raw JSON),
// not Parameters (*genai.Schema). We prefer ParametersJsonSchema when available
// because it preserves the original JSON Schema format that Hugr expects.
func adkToHugrTools(genaiTools []*genai.Tool) ([]string, error) {
	if len(genaiTools) == 0 {
		return nil, nil
	}

	var result []string
	for _, t := range genaiTools {
		if t == nil || len(t.FunctionDeclarations) == 0 {
			continue
		}
		for _, decl := range t.FunctionDeclarations {
			// Prefer ParametersJsonSchema (raw JSON Schema from MCP)
			// over Parameters (*genai.Schema which uses UPPERCASE types).
			// For genai.Schema, convert to raw map to normalize type names
			// (OBJECT→object, STRING→string) since Hugr expects OpenAI format.
			var params any
			if decl.ParametersJsonSchema != nil {
				params = decl.ParametersJsonSchema
			} else if decl.Parameters != nil {
				params = schemaToMap(decl.Parameters)
			}
			// OpenAI/Hugr requires parameters to be a valid JSON Schema object.
			// Must include "type", "properties", and "required" — some model
			// Jinja templates (Gemma4) crash on undefined fields.
			if params == nil {
				params = map[string]any{
					"type":       "object",
					"properties": map[string]any{},
					"required":   []string{},
				}
			}

			hugrTool := types.LLMTool{
				Name:        decl.Name,
				Description: decl.Description,
				Parameters:  params,
			}
			b, err := json.Marshal(hugrTool)
			if err != nil {
				return nil, fmt.Errorf("marshal tool %q: %w", decl.Name, err)
			}
			result = append(result, string(b))
		}
	}
	return result, nil
}

// hugrResultToADKContent converts a types.LLMResult to ADK Content.
func hugrResultToADKContent(result types.LLMResult) *genai.Content {
	var parts []*genai.Part

	if result.Content != "" {
		parts = append(parts, &genai.Part{Text: result.Content})
	}

	for i, tc := range result.ToolCalls {
		args := normalizeArgs(tc.Arguments)
		part := &genai.Part{
			FunctionCall: &genai.FunctionCall{
				ID:   tc.ID,
				Name: tc.Name,
				Args: args,
			},
		}
		// Gemini 2.5+: ThoughtSignature goes on the first functionCall Part only.
		if i == 0 && result.ThoughtSignature != "" {
			part.ThoughtSignature = []byte(result.ThoughtSignature)
		}
		parts = append(parts, part)
	}

	if len(parts) == 0 {
		parts = append(parts, &genai.Part{Text: ""})
	}

	return &genai.Content{
		Role:  "model",
		Parts: parts,
	}
}

func normalizeArgs(v any) map[string]any {
	switch args := v.(type) {
	case map[string]any:
		return args
	case string:
		var m map[string]any
		if err := json.Unmarshal([]byte(args), &m); err != nil {
			return map[string]any{"raw": args}
		}
		return m
	default:
		if v == nil {
			return nil
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			return map[string]any{"raw": string(b)}
		}
		return m
	}
}

// schemaToMap converts genai.Schema to a JSON Schema map with lowercase types.
// ADK uses UPPERCASE type names (OBJECT, STRING) but Hugr/OpenAI expects lowercase.
func schemaToMap(s *genai.Schema) map[string]any {
	if s == nil {
		return nil
	}
	m := map[string]any{
		"type": strings.ToLower(string(s.Type)),
	}
	if s.Description != "" {
		m["description"] = s.Description
	}
	if len(s.Required) > 0 {
		m["required"] = s.Required
	}
	if len(s.Enum) > 0 {
		m["enum"] = s.Enum
	}
	if s.Properties != nil {
		props := make(map[string]any, len(s.Properties))
		for k, v := range s.Properties {
			props[k] = schemaToMap(v)
		}
		m["properties"] = props
	}
	if s.Items != nil {
		m["items"] = schemaToMap(s.Items)
	}
	return m
}

// mapFinishReason converts Hugr finish_reason to ADK FinishReason.
func mapFinishReason(reason string) genai.FinishReason {
	switch reason {
	case "stop":
		return genai.FinishReasonStop
	case "length", "max_tokens":
		return genai.FinishReasonMaxTokens
	case "tool_use":
		return genai.FinishReasonStop
	default:
		return genai.FinishReasonOther
	}
}
