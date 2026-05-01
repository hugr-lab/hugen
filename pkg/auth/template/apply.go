package template

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Context carries the values Apply substitutes for placeholders.
// UserID and Role come from identity.WhoAmI; AgentID is the
// runtime agent id; Session* come from the per-call ctx via
// pkg/auth/perm.SessionFromContext.
type Context struct {
	UserID          string
	Role            string
	AgentID         string
	SessionID       string
	SessionMetadata map[string]string
}

// Apply substitutes placeholders inside JSON string values:
//
//   - [$auth.user_id]   — WhoAmI.UserID (token bearer)
//   - [$auth.role]      — WhoAmI.Role
//   - [$agent.id]       — runtime agent id
//   - [$session.id]
//   - [$session.metadata.<key>]
//
// Substitution is one-pass — a placeholder produced by
// substitution is preserved literally rather than re-substituted —
// so the transform is terminating and predictable. Missing keys
// substitute to the empty string. Malformed `[$...]` tokens are
// preserved verbatim.
//
// Non-string JSON values (numbers, bools, null, arrays, objects)
// pass through unchanged at the leaf level — Apply walks
// recursively and only rewrites string leaves.
func Apply(raw json.RawMessage, ctx Context) (json.RawMessage, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("template: unmarshal: %w", err)
	}
	v = walk(v, ctx)
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("template: marshal: %w", err)
	}
	return json.RawMessage(out), nil
}

// ApplyString runs Apply on a single string value (no JSON wrap).
// Convenient for Filter strings that aren't wrapped in a JSON
// envelope.
func ApplyString(s string, ctx Context) string {
	return substitute(s, ctx)
}

func walk(v any, ctx Context) any {
	switch t := v.(type) {
	case string:
		return substitute(t, ctx)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = walk(vv, ctx)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = walk(vv, ctx)
		}
		return out
	default:
		return v
	}
}

// substitute replaces every recognised `[$...]` token. One pass,
// no re-substitution of the produced text.
func substitute(s string, ctx Context) string {
	if !strings.Contains(s, "[$") {
		return s
	}
	var b bytes.Buffer
	i := 0
	for i < len(s) {
		j := strings.Index(s[i:], "[$")
		if j < 0 {
			b.WriteString(s[i:])
			break
		}
		b.WriteString(s[i : i+j])
		end := strings.Index(s[i+j:], "]")
		if end < 0 {
			// malformed — no closing bracket; copy rest verbatim.
			b.WriteString(s[i+j:])
			break
		}
		token := s[i+j+2 : i+j+end] // strip "[$" and trailing "]"
		val, ok := lookup(token, ctx)
		if ok {
			b.WriteString(val)
		} else {
			// unknown / malformed — preserve verbatim.
			b.WriteString(s[i+j : i+j+end+1])
		}
		i = i + j + end + 1
	}
	return b.String()
}

func lookup(token string, ctx Context) (string, bool) {
	switch token {
	case "auth.user_id":
		return ctx.UserID, true
	case "auth.role":
		return ctx.Role, true
	case "agent.id":
		return ctx.AgentID, true
	case "session.id":
		return ctx.SessionID, true
	}
	if rest, ok := strings.CutPrefix(token, "session.metadata."); ok && rest != "" {
		if ctx.SessionMetadata == nil {
			return "", true
		}
		return ctx.SessionMetadata[rest], true
	}
	return "", false
}
