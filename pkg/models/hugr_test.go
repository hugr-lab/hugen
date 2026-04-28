package models_test

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/query-engine/client"

	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/models"
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

func drain(t *testing.T, stream model.Stream) []model.Chunk {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var out []model.Chunk
	for {
		ch, more, err := stream.Next(ctx)
		if err != nil {
			t.Fatalf("stream.Next: %v", err)
		}
		if !more {
			return out
		}
		out = append(out, ch)
	}
}

func TestHugrModel_Generate_StreamsChunks(t *testing.T) {
	skipWithoutHugr(t)

	m := models.NewHugr(testClient, testModel, models.WithLogger(slog.Default()))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	stream, err := m.Generate(ctx, model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: "Say hello in one word"}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	defer stream.Close()
	chunks := drain(t, stream)

	if len(chunks) == 0 {
		t.Fatal("no chunks received")
	}
	if !chunks[len(chunks)-1].Final {
		t.Errorf("last chunk not Final: %+v", chunks[len(chunks)-1])
	}
	for i, ch := range chunks[:len(chunks)-1] {
		if ch.Final {
			t.Errorf("chunk %d marked Final but is not last", i)
		}
	}
}

// TestHugrModel_Generate_NoTurnCompleteDuplication asserts that the
// final (Final=true) chunk does NOT carry duplicated content from the
// streamed deltas. Closes phase-1 review carry-over: the ADK bridge
// used to suppress this duplicate manually; the native path simply
// never produces it. (SC-012)
func TestHugrModel_Generate_NoTurnCompleteDuplication(t *testing.T) {
	skipWithoutHugr(t)

	m := models.NewHugr(testClient, testModel, models.WithLogger(slog.Default()))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	stream, err := m.Generate(ctx, model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: "Say hello in one word"}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	defer stream.Close()
	chunks := drain(t, stream)
	if len(chunks) == 0 {
		t.Fatal("no chunks received")
	}
	last := chunks[len(chunks)-1]
	if !last.Final {
		t.Fatal("last chunk not Final")
	}
	// The final chunk must carry only metadata: Final=true, optional
	// Usage. It must NOT carry Content or Reasoning.
	if last.Content != nil {
		t.Errorf("final chunk leaked Content: %q", *last.Content)
	}
	if last.Reasoning != nil {
		t.Errorf("final chunk leaked Reasoning: %q", *last.Reasoning)
	}
}

// TestHugrModel_StreamClose_CancelsUpstream asserts Close() actually
// propagates cancellation to the subscription's ctx. We force the
// situation by Close()-ing the stream before draining it; subsequent
// Next() must return either ctx.Canceled or (Chunk{}, false, nil).
// (FR-022 / SC-008)
func TestHugrModel_StreamClose_CancelsUpstream(t *testing.T) {
	skipWithoutHugr(t)

	m := models.NewHugr(testClient, testModel, models.WithLogger(slog.Default()))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	stream, err := m.Generate(ctx, model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: "Write a long essay"}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// Read at least one chunk to confirm the stream is live.
	if _, _, err := stream.Next(ctx); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// After Close, subsequent reads MUST not block: either the
	// channel is drained quickly or returns immediately with a
	// cancellation error.
	done := make(chan struct{})
	go func() {
		readCtx, readCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer readCancel()
		for {
			_, more, err := stream.Next(readCtx)
			if err != nil || !more {
				close(done)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not drain after Close within 5s")
	}
}

func TestHugrModel_Name(t *testing.T) {
	c := client.NewClient("http://localhost:15000/ipc")
	m := models.NewHugr(c, "test-model")
	if m.Name() != "hugr-model" {
		t.Errorf("default Name() = %q, want hugr-model", m.Name())
	}
	m2 := models.NewHugr(c, "test-model", models.WithName("custom"))
	if m2.Name() != "custom" {
		t.Errorf("override Name() = %q", m2.Name())
	}
}

// Helper to keep the linter happy — keeps strings.Contains import
// available for any future test that asserts log lines.
var _ = strings.Contains
