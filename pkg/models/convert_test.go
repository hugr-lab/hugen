package models

import (
	"testing"

	"github.com/hugr-lab/query-engine/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genai"
)

func TestAdkToHugrMessages(t *testing.T) {
	tests := []struct {
		name     string
		contents []*genai.Content
		wantLen  int
		wantErr  bool
	}{
		{
			name: "single user text message",
			contents: []*genai.Content{
				{Role: "user", Parts: []*genai.Part{{Text: "hello"}}},
			},
			wantLen: 1,
		},
		{
			name: "multi-turn conversation",
			contents: []*genai.Content{
				{Role: "user", Parts: []*genai.Part{{Text: "what is 2+2?"}}},
				{Role: "model", Parts: []*genai.Part{{Text: "4"}}},
				{Role: "user", Parts: []*genai.Part{{Text: "thanks"}}},
			},
			wantLen: 3,
		},
		{
			name: "model with function call",
			contents: []*genai.Content{
				{Role: "model", Parts: []*genai.Part{{
					FunctionCall: &genai.FunctionCall{
						ID:   "call_1",
						Name: "search",
						Args: map[string]any{"query": "test"},
					},
				}}},
			},
			wantLen: 1,
		},
		{
			name: "function response",
			contents: []*genai.Content{
				{Role: "function", Parts: []*genai.Part{{
					FunctionResponse: &genai.FunctionResponse{
						ID:       "call_1",
						Name:     "search",
						Response: map[string]any{"result": "found"},
					},
				}}},
			},
			wantLen: 1,
		},
		{
			name:     "empty contents",
			contents: nil,
			wantLen:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgs, err := adkToHugrMessages(tt.contents)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Len(t, msgs, tt.wantLen)
		})
	}
}

func TestAdkToHugrMessages_RoleMapping(t *testing.T) {
	contents := []*genai.Content{
		{Role: "user", Parts: []*genai.Part{{Text: "hi"}}},
		{Role: "model", Parts: []*genai.Part{{Text: "hello"}}},
	}

	msgs, err := adkToHugrMessages(contents)
	require.NoError(t, err)
	require.Len(t, msgs, 2)

	assert.Contains(t, msgs[0], `"role":"user"`)
	assert.Contains(t, msgs[1], `"role":"assistant"`)
}

func TestAdkToHugrTools(t *testing.T) {
	tests := []struct {
		name    string
		tools   []*genai.Tool
		wantLen int
	}{
		{
			name:    "nil tools",
			tools:   nil,
			wantLen: 0,
		},
		{
			name:    "empty slice",
			tools:   []*genai.Tool{},
			wantLen: 0,
		},
		{
			name: "single tool declaration",
			tools: []*genai.Tool{{
				FunctionDeclarations: []*genai.FunctionDeclaration{{
					Name:        "search",
					Description: "Search for data",
				}},
			}},
			wantLen: 1,
		},
		{
			name: "multiple declarations in one tool",
			tools: []*genai.Tool{{
				FunctionDeclarations: []*genai.FunctionDeclaration{
					{Name: "tool1"},
					{Name: "tool2"},
					{Name: "tool3"},
				},
			}},
			wantLen: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := adkToHugrTools(tt.tools)
			require.NoError(t, err)
			assert.Len(t, result, tt.wantLen)
		})
	}
}

func TestHugrResultToADKContent(t *testing.T) {
	tests := []struct {
		name          string
		result        types.LLMResult
		wantPartsLen  int
		wantText      string
		wantFuncCalls int
	}{
		{
			name: "text only response",
			result: types.LLMResult{
				Content: "Hello, world!",
			},
			wantPartsLen:  1,
			wantText:      "Hello, world!",
			wantFuncCalls: 0,
		},
		{
			name: "tool call response",
			result: types.LLMResult{
				Content: "",
				ToolCalls: []types.LLMToolCall{
					{ID: "call_1", Name: "search", Arguments: map[string]any{"q": "test"}},
				},
			},
			wantPartsLen:  1,
			wantFuncCalls: 1,
		},
		{
			name: "text plus tool call",
			result: types.LLMResult{
				Content: "Let me search for that.",
				ToolCalls: []types.LLMToolCall{
					{ID: "call_1", Name: "search", Arguments: map[string]any{"q": "data"}},
				},
			},
			wantPartsLen:  2,
			wantText:      "Let me search for that.",
			wantFuncCalls: 1,
		},
		{
			name:          "empty response",
			result:        types.LLMResult{},
			wantPartsLen:  1,
			wantText:      "",
			wantFuncCalls: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := hugrResultToADKContent(tt.result)
			require.NotNil(t, content)
			assert.Equal(t, "model", content.Role)
			assert.Len(t, content.Parts, tt.wantPartsLen)

			var funcCalls int
			for _, p := range content.Parts {
				if p.FunctionCall != nil {
					funcCalls++
				}
				if tt.wantText != "" && p.Text != "" {
					assert.Equal(t, tt.wantText, p.Text)
				}
			}
			assert.Equal(t, tt.wantFuncCalls, funcCalls)
		})
	}
}

func TestMapFinishReason(t *testing.T) {
	tests := []struct {
		hugr string
		want genai.FinishReason
	}{
		{"stop", genai.FinishReasonStop},
		{"length", genai.FinishReasonMaxTokens},
		{"max_tokens", genai.FinishReasonMaxTokens},
		{"tool_use", genai.FinishReasonStop},
		{"unknown", genai.FinishReasonOther},
		{"", genai.FinishReasonOther},
	}

	for _, tt := range tests {
		t.Run(tt.hugr, func(t *testing.T) {
			got := mapFinishReason(tt.hugr)
			assert.Equal(t, tt.want, got)
		})
	}
}
