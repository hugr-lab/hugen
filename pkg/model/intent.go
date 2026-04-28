package model

// Intent classifies the LLM task category for routing. The
// runtime declares a small enum; new intents can be added as the
// agent grows.
type Intent string

const (
	IntentDefault     Intent = "default"
	IntentCheap       Intent = "cheap"
	IntentToolCalling Intent = "tool_calling"
	IntentSummarize   Intent = "summarize"
)

// ModelSpec uniquely identifies a configured model.
//
// Provider is the family ("hugr", "openai", ...). Name is the
// provider-specific model name. The pair is the registry key.
type ModelSpec struct {
	Provider string
	Name     string
}

func (s ModelSpec) String() string {
	return s.Provider + "/" + s.Name
}

// Hint is the input to ModelRouter.Resolve. Higher-priority fields
// (ModelOverride) are tried first; lower-priority maps form the
// 5-step lookup chain.
type Hint struct {
	Intent        Intent
	ModelOverride *ModelSpec
	SessionModels map[Intent]ModelSpec
	SkillModels   map[Intent]ModelSpec
}
