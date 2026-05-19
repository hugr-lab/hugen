package mission

import (
	"encoding/json"
	"time"
)

// jsonMarshalIndent encodes v with a 2-space indent. Used by
// first-message renderers that include structured body payloads
// (depends_on resolution, plan AST display) so the worker sees
// pretty JSON rather than packed bytes. Errors propagate to the
// caller — the renderer is expected to handle unrenderable bodies.
func jsonMarshalIndent(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// nowFn returns the current wall-clock time. Var-form so tests can
// pin it to a fixed instant via setNow / restoreNow without
// reaching into time.Now from every call site.
var nowFn = time.Now

// setNow temporarily replaces nowFn for the duration of a test
// scope. Returns a closer the caller calls in defer.
//
// Not safe for parallel tests — kept here rather than exported
// because the package-internal callers are the only users.
func setNow(t time.Time) (restore func()) {
	prev := nowFn
	nowFn = func() time.Time { return t }
	return func() { nowFn = prev }
}
