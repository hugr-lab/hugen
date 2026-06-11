package compactor

import (
	"context"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/model"
)

// summariseInput is the template binding for
// `assets/prompts/compactor/summarise.tmpl`. Every text field
// is pre-truncated in Go (the Renderer has no FuncMap).
type summariseInput struct {
	PriorBlocks     []SummaryBlock
	KeptVerbatim    []KeptSection
	ToolCalls       []toolCallPair
	InquirySegments []inquirySegment
	From, To        int64
	MaxTokens       int
}

// collapseInput is the binding for
// `assets/prompts/compactor/collapse.tmpl`.
type collapseInput struct {
	PriorBlocks  []SummaryBlock
	KeptVerbatim []KeptSection
	MaxTokens    int
}

// runSummariser builds the summariser prompt, resolves the
// configured intent through the router, streams the model
// response, and returns the trimmed body. Errors bubble back
// so the caller can apply the hard-fallback marker (spec §5.6).
//
// γ: cfg is the resolved per-tier / per-skill / per-role config
// produced by [Extension.resolveTierConfig]. LLMTimeout +
// LLMIntent are read from cfg so a per-role override (e.g. a
// cheap intent for a fast worker) lands without recompiling.
func (e *Extension) runSummariser(ctx context.Context, state extension.SessionState, cfg Config, prior *DigestPayload, c classified, fromSeq, toSeq int64) (string, error) {
	if state.Prompts() == nil {
		return "", fmt.Errorf("prompts renderer not available")
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, cfg.LLMTimeout)
	defer cancel()

	mdl, _, err := e.deps.Router.Resolve(timeoutCtx, model.Hint{Intent: cfg.LLMIntent})
	if err != nil {
		return "", fmt.Errorf("resolve model: %w", err)
	}

	var priorBlocks []SummaryBlock
	if prior != nil {
		// Truncate each prior block to bound the re-prompt
		// context. We copy so the caller's slice is not mutated.
		priorBlocks = make([]SummaryBlock, len(prior.SummaryBlocks))
		for i, b := range prior.SummaryBlocks {
			priorBlocks[i] = SummaryBlock{
				Iter: b.Iter,
				From: b.From,
				To:   b.To,
				Text: truncateForPrior(b.Text),
			}
		}
	}

	body, err := state.Prompts().Render("compactor/summarise", summariseInput{
		PriorBlocks:     priorBlocks,
		KeptVerbatim:    c.kept,
		ToolCalls:       c.toolPairs,
		InquirySegments: c.inquiries,
		From:            fromSeq,
		To:              toSeq,
		MaxTokens:       summariseMaxTokens,
	})
	if err != nil {
		return "", fmt.Errorf("render summarise: %w", err)
	}

	return extension.StreamModelText(timeoutCtx, mdl, body, summariseMaxTokens)
}

// runCollapse runs the cap-driven collapse LLM call: takes the
// current SummaryBlocks + KeptVerbatim shadow and asks the
// model to fold them into a single replacement block. Returns
// the trimmed body or an error.
//
// γ: cfg supplies the resolved LLMTimeout + LLMIntent — same
// reasoning as runSummariser.
func (e *Extension) runCollapse(ctx context.Context, state extension.SessionState, cfg Config, blocks []SummaryBlock, kept []KeptSection) (string, error) {
	if state.Prompts() == nil {
		return "", fmt.Errorf("prompts renderer not available")
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, cfg.LLMTimeout)
	defer cancel()

	mdl, _, err := e.deps.Router.Resolve(timeoutCtx, model.Hint{Intent: cfg.LLMIntent})
	if err != nil {
		return "", fmt.Errorf("resolve model: %w", err)
	}

	// Truncate per-block to bound re-prompt size.
	priorBlocks := make([]SummaryBlock, len(blocks))
	for i, b := range blocks {
		priorBlocks[i] = SummaryBlock{
			Iter: b.Iter,
			From: b.From,
			To:   b.To,
			Text: truncateForPrior(b.Text),
		}
	}

	body, err := state.Prompts().Render("compactor/collapse", collapseInput{
		PriorBlocks:  priorBlocks,
		KeptVerbatim: kept,
		MaxTokens:    summariseMaxTokens,
	})
	if err != nil {
		return "", fmt.Errorf("render collapse: %w", err)
	}
	return extension.StreamModelText(timeoutCtx, mdl, body, summariseMaxTokens)
}
