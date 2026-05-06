package runtime

import (
	"errors"
	"fmt"
)

// ErrInvalidConfig signals a Config that fails Validate. Build
// returns it (wrapped with the failing field) before touching any
// resource.
var ErrInvalidConfig = errors.New("runtime: invalid config")

// errInvalid wraps ErrInvalidConfig with a human-readable detail.
func errInvalid(detail string) error {
	return fmt.Errorf("%w: %s", ErrInvalidConfig, detail)
}
