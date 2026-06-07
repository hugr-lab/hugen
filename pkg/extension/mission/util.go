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

// nowFn returns the current wall-clock time. Var-form so a test can
// pin it to a fixed instant by swapping the var, without reaching
// into time.Now from every call site.
var nowFn = time.Now
