package session

import (
	"encoding/json"
	"testing"
)

// TestRecoverStringifiedClarifications covers the weak-model salvage
// path: a clarifications array passed as a JSON-encoded string instead
// of a real array. Happy path recovers + preserves sibling fields;
// every non-stringified shape returns ok=false so the caller keeps its
// strict bad_request behaviour.
func TestRecoverStringifiedClarifications(t *testing.T) {
	t.Run("recovers stringified array + preserves siblings", func(t *testing.T) {
		// The exact shape a weak model emitted in dogfood: clarifications
		// is a JSON STRING wrapping the array.
		inner := `[{"id":"module_choice","kind":"required","options":["synthea","op2023","Other"],"question":"Which module?"}]`
		blob, _ := json.Marshal(inner) // -> a JSON string literal
		args := json.RawMessage(`{"type":"clarification","question":"pick","clarifications":` + string(blob) + `}`)

		got, ok := recoverStringifiedClarifications(args)
		if !ok {
			t.Fatalf("expected recovery to succeed")
		}
		if got.Type != "clarification" || got.Question != "pick" {
			t.Fatalf("sibling fields not preserved: type=%q question=%q", got.Type, got.Question)
		}
		if len(got.Clarifications) != 1 {
			t.Fatalf("clarifications len = %d, want 1", len(got.Clarifications))
		}
		c := got.Clarifications[0]
		if c.ID != "module_choice" || c.Question != "Which module?" || len(c.Options) != 3 {
			t.Fatalf("clarification not decoded: %+v", c)
		}
	})

	t.Run("real array is NOT recovered (strict path owns it)", func(t *testing.T) {
		args := json.RawMessage(`{"type":"clarification","clarifications":[{"id":"x","question":"q"}]}`)
		if _, ok := recoverStringifiedClarifications(args); ok {
			t.Fatalf("real array should return ok=false — strict unmarshal already handles it")
		}
	})

	t.Run("absent clarifications returns false", func(t *testing.T) {
		args := json.RawMessage(`{"type":"clarification","question":"q","options":["a","b"]}`)
		if _, ok := recoverStringifiedClarifications(args); ok {
			t.Fatalf("no clarifications field should return ok=false")
		}
	})

	t.Run("stringified non-array does not recover", func(t *testing.T) {
		args := json.RawMessage(`{"type":"clarification","clarifications":"not an array"}`)
		if _, ok := recoverStringifiedClarifications(args); ok {
			t.Fatalf("stringified non-array should return ok=false")
		}
	})

	t.Run("malformed top-level json returns false", func(t *testing.T) {
		if _, ok := recoverStringifiedClarifications(json.RawMessage(`{not json`)); ok {
			t.Fatalf("malformed json should return ok=false")
		}
	})
}
