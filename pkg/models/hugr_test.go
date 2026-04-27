package models_test

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/models"
	"github.com/hugr-lab/query-engine/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

var (
	testClient *client.Client
	testModel  string
)

func TestMain(m *testing.M) {
	url := os.Getenv("HUGR_URL")
	key := os.Getenv("HUGR_SECRET_KEY")
	testModel = os.Getenv("AGENT_MODEL")

	if url != "" && key != "" {
		testClient = client.NewClient(
			url+"/ipc",
			client.WithSecretKeyAuth(key),
		)
		if testModel == "" {
			testModel = "gemma-small"
		}
	}

	os.Exit(m.Run())
}

func skipWithoutHugr(t *testing.T) {
	t.Helper()
	if testClient == nil {
		t.Skip("HUGR_URL and HUGR_SECRET_KEY not set, skipping integration test")
	}
}

func TestHugrModel_SimpleCompletion(t *testing.T) {
	skipWithoutHugr(t)

	m := models.NewHugr(testClient, testModel,
		models.WithLogger(slog.Default()),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	req := &model.LLMRequest{
		Contents: []*genai.Content{
			{Role: "user", Parts: []*genai.Part{{Text: "Say hello in one word"}}},
		},
		Config: &genai.GenerateContentConfig{
			MaxOutputTokens: 200,
		},
	}

	var responses []*model.LLMResponse
	for resp, err := range m.GenerateContent(ctx, req, true) {
		require.NoError(t, err)
		responses = append(responses, resp)
	}

	require.NotEmpty(t, responses, "should receive at least one response")

	// Last response must be TurnComplete.
	last := responses[len(responses)-1]
	assert.True(t, last.TurnComplete, "last response should be TurnComplete")
	assert.NotNil(t, last.Content, "last response should have content")
	assert.NotEmpty(t, last.Content.Parts, "last response should have parts")
	assert.Equal(t, "model", last.Content.Role)

	// Should have partial streaming responses before the final one.
	if len(responses) > 1 {
		for _, r := range responses[:len(responses)-1] {
			assert.True(t, r.Partial, "non-final response should be Partial")
		}
	}

	// Usage metadata should be present in the final response.
	// Note: some models (e.g. gemma-small) may return 0 tokens in the finish event.
	if last.UsageMetadata != nil {
		t.Logf("tokens: prompt=%d completion=%d",
			last.UsageMetadata.PromptTokenCount,
			last.UsageMetadata.CandidatesTokenCount,
		)
	}

	// Collect full text (content + thoughts) from all responses.
	var contentText, thoughtText string
	for _, r := range responses {
		if r.Content != nil {
			for _, p := range r.Content.Parts {
				if p.Thought {
					thoughtText += p.Text
				} else if p.Text != "" {
					contentText += p.Text
				}
			}
		}
	}
	// At least one of content or thought should be non-empty.
	assert.True(t, contentText != "" || thoughtText != "",
		"should have generated text or thought content")
	t.Logf("content: %q, thought length: %d", contentText, len(thoughtText))
}

func TestHugrModel_MultiTurnConversation(t *testing.T) {
	skipWithoutHugr(t)

	m := models.NewHugr(testClient, testModel,
		models.WithLogger(slog.Default()),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	req := &model.LLMRequest{
		Contents: []*genai.Content{
			{Role: "user", Parts: []*genai.Part{{Text: "Remember the number 42"}}},
			{Role: "model", Parts: []*genai.Part{{Text: "I'll remember the number 42."}}},
			{Role: "user", Parts: []*genai.Part{{Text: "What number did I ask you to remember?"}}},
		},
		Config: &genai.GenerateContentConfig{
			MaxOutputTokens: 50,
		},
	}

	var lastResp *model.LLMResponse
	for resp, err := range m.GenerateContent(ctx, req, true) {
		require.NoError(t, err)
		lastResp = resp
	}

	require.NotNil(t, lastResp)
	assert.True(t, lastResp.TurnComplete)

	var fullText string
	for resp, err := range m.GenerateContent(ctx, req, true) {
		require.NoError(t, err)
		if resp.Content != nil {
			for _, p := range resp.Content.Parts {
				if p.Text != "" {
					fullText += p.Text
				}
			}
		}
	}
	assert.Contains(t, fullText, "42", "response should contain the remembered number")
	t.Logf("response: %q", fullText)
}

func TestHugrModel_ContextCancellation(t *testing.T) {
	skipWithoutHugr(t)

	m := models.NewHugr(testClient, testModel,
		models.WithLogger(slog.Default()),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	// Give the context time to expire.
	time.Sleep(5 * time.Millisecond)

	req := &model.LLMRequest{
		Contents: []*genai.Content{
			{Role: "user", Parts: []*genai.Part{{Text: "Write a very long essay about the history of computing"}}},
		},
		Config: &genai.GenerateContentConfig{
			MaxOutputTokens: 4096,
		},
	}

	var gotError bool
	for _, err := range m.GenerateContent(ctx, req, true) {
		if err != nil {
			gotError = true
			break
		}
	}
	assert.True(t, gotError, "should get error with cancelled context")
}

func TestHugrModel_Name(t *testing.T) {
	c := client.NewClient("http://localhost:15000/ipc")
	m := models.NewHugr(c, "test-model")
	assert.Equal(t, "hugr-model", m.Name())

	m2 := models.NewHugr(c, "test-model", models.WithName("custom"))
	assert.Equal(t, "custom", m2.Name())
}
