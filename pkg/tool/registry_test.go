package tool

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

// discardLogger is a small helper used across pkg/tool tests.
func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestInit_NilView confirms Init is a no-op when the manager has
// no view wired (tests, deployments without per_agent providers).
//
// The integration tests that exercise Init through the production
// providers.Builder live in pkg/tool/providers; pkg/tool keeps only
// the trivial nil-view case here because the wider Init coverage
// requires pkg/tool/providers/mcp, which would create an import
// cycle (pkg/tool cannot depend on its own provider subpackages).
func TestInit_NilView(t *testing.T) {
	tm := NewToolManager(nil, nil, discardLogger())
	t.Cleanup(func() { _ = tm.Close() })
	if err := tm.Init(context.Background()); err != nil {
		t.Fatalf("expected no-op, got %v", err)
	}
}
