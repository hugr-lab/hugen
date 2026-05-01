package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vektah/gqlparser/v2/gqlerror"
)

// toolError is the structured error envelope every tool handler
// surfaces. The ToolManager translates the JSON body into a
// `tool_error{code:..}` Frame on the client side.
type toolError struct {
	Code         string                 `json:"code"`
	Msg          string                 `json:"message,omitempty"`
	ElapsedMS    int                    `json:"elapsed_ms,omitempty"`
	GraphQLErrs  []map[string]any       `json:"graphql_errors,omitempty"`
	Extra        map[string]any         `json:"extra,omitempty"`
}

func (e *toolError) Error() string {
	if e.Msg == "" {
		return fmt.Sprintf("tool_error: %s", e.Code)
	}
	return fmt.Sprintf("tool_error{%s}: %s", e.Code, e.Msg)
}

// mapClientError converts a *client.Client.Query error into a
// toolError. We treat context-deadline as the timeout case (the
// runtime contract), 401-shaped errors as auth, and anything else
// generic.
func mapClientError(ctx context.Context, err error, elapsed time.Duration) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &toolError{
			Code:      "timeout",
			ElapsedMS: int(elapsed / time.Millisecond),
			Msg:       "query exceeded effective deadline",
		}
	}
	msg := err.Error()
	if strings.Contains(msg, "401") || strings.Contains(strings.ToLower(msg), "unauthorized") {
		return &toolError{Code: "auth", Msg: msg}
	}
	return &toolError{Code: "hugr_error", Msg: msg}
}

// mapJQError is mapClientError plus a jq_error pass for jq syntax
// failures. The query-engine surfaces those as a generic error
// containing "jq:" prefix in the message.
func mapJQError(ctx context.Context, err error, elapsed time.Duration) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &toolError{
			Code:      "timeout",
			ElapsedMS: int(elapsed / time.Millisecond),
		}
	}
	msg := err.Error()
	if strings.Contains(strings.ToLower(msg), "jq") {
		return &toolError{Code: "jq_error", Msg: msg}
	}
	return mapClientError(ctx, err, elapsed)
}

// hugrError formats a GraphQL error list as a tool_error.
func hugrError(list gqlerror.List) error {
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		entry := map[string]any{"message": e.Message}
		if e.Path != nil {
			entry["path"] = e.Path
		}
		if len(e.Extensions) > 0 {
			entry["extensions"] = e.Extensions
		}
		out = append(out, entry)
	}
	return &toolError{Code: "hugr_error", GraphQLErrs: out}
}

