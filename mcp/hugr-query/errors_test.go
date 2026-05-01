package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

func TestMapClientError_Timeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Context cancelled, but DeadlineExceeded only triggers on real
	// deadlines — pass it explicitly.
	got := mapClientError(ctx, context.DeadlineExceeded, 200*time.Millisecond)
	var te *toolError
	if !errors.As(got, &te) || te.Code != "timeout" {
		t.Fatalf("got %v want timeout toolError", got)
	}
	if te.ElapsedMS != 200 {
		t.Fatalf("elapsed=%d", te.ElapsedMS)
	}
}

func TestMapClientError_Auth(t *testing.T) {
	got := mapClientError(context.Background(), errors.New("HTTP 401 Unauthorized"), 0)
	var te *toolError
	if !errors.As(got, &te) || te.Code != "auth" {
		t.Fatalf("got %v want auth", got)
	}
}

func TestMapClientError_Generic(t *testing.T) {
	got := mapClientError(context.Background(), errors.New("connection refused"), 0)
	var te *toolError
	if !errors.As(got, &te) || te.Code != "hugr_error" {
		t.Fatalf("got %v want hugr_error", got)
	}
}

func TestMapJQError_JQClassified(t *testing.T) {
	got := mapJQError(context.Background(), errors.New("jq: error: invalid syntax"), 0)
	var te *toolError
	if !errors.As(got, &te) || te.Code != "jq_error" {
		t.Fatalf("got %v want jq_error", got)
	}
}

func TestHugrError_FormatsList(t *testing.T) {
	list := gqlerror.List{
		{Message: "field x not found"},
		{Message: "permission denied", Path: ast.Path{ast.PathName("users"), ast.PathIndex(0)}},
	}
	got := hugrError(list)
	var te *toolError
	if !errors.As(got, &te) || te.Code != "hugr_error" {
		t.Fatalf("got %v want hugr_error", got)
	}
	if len(te.GraphQLErrs) != 2 {
		t.Fatalf("graphql_errors=%d want 2", len(te.GraphQLErrs))
	}
	if te.GraphQLErrs[1]["path"] == nil {
		t.Fatalf("path missing on second entry")
	}
}

