package model

import (
	"context"
)

// Role labels for Message.Role.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleSystem    = "system"
	RoleTool      = "tool"
)

// Model is the runtime-side model interface. Concrete providers
// (e.g. *models.HugrModel) implement it via structural typing.
//
// Generate returns a stream of Chunks. Implementations must close
// the stream cleanly when ctx is cancelled.
type Model interface {
	Spec() ModelSpec
	Generate(ctx context.Context, req Request) (Stream, error)
}

// Stream yields chunks from a single model call.
type Stream interface {
	// Next returns (chunk, more, err). more=false signals the stream
	// is exhausted (chunk may still be valid — typically the final
	// usage chunk).
	Next(ctx context.Context) (Chunk, bool, error)
	Close() error
}

// Request carries the full input to a single model call.
type Request struct {
	Messages    []Message
	Tools       []Tool
	MaxTokens   int
	Temperature *float32
}

// Message is one turn in the conversation history sent to the
// model.
type Message struct {
	Role    string // user | assistant | system | tool
	Content string
	// Reserved for future multimodal Parts.
	Parts []Part
	// Optional: for role=tool, the tool call id this is a response to.
	ToolCallID string
}

// Part is a placeholder for future multimodal content (images,
// audio). Phase 1 only uses Message.Content.
type Part struct {
	Kind string // "text" | "image" | ...
	Text string
}

// Tool is a phase-3 placeholder. Phase 1 never populates
// Request.Tools, but the type exists so model providers can plumb
// it through without an interface change later.
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any
}

// Chunk is a single emission from a streamed model call.
//
// Reasoning and Content are mutually exclusive in practice
// (providers emit either thought tokens or content tokens per
// chunk). Usage is populated on the final chunk.
type Chunk struct {
	Reasoning *string
	Content   *string
	ToolCall  *ChunkToolCall
	Usage     *Usage
	Final     bool
}

// ChunkToolCall is a streamed tool-call request from the model.
type ChunkToolCall struct {
	ID   string
	Name string
	Args any
}

// Usage carries token counts emitted on the final chunk.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}
