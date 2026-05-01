package perm

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/hugr-lab/hugen/pkg/config"
)

// Rule is the in-memory representation of one entry from operator
// configuration (Tier 1) or from function.core.auth.my_permissions
// (Tier 2). The shape is identical; the source determines merge
// precedence (config wins on Data scalar conflict).
//
// Type aliases pkg/config.PermissionRule so callers don't have to
// hold both names.
type Rule = config.PermissionRule

// Permission is the merged decision for one (object, field) at
// decision time. ToolManager.Resolve fills FromUser after Tier-3
// is consulted.
type Permission struct {
	Disabled   bool
	Hidden     bool
	Data       json.RawMessage
	Filter     string
	FromConfig bool
	FromRemote bool
	FromUser   bool
}

// RefreshEvent is what Subscribe streams. Useful for ToolManager
// to bump its policy_gen on TTL expiry.
type RefreshEvent struct {
	At         time.Time
	Generation int64
	Err        error
}

// Errors. Sentinel values, errors.Is-comparable.
var (
	// ErrPermissionDenied is returned by Resolve when any tier
	// disabled the call.
	ErrPermissionDenied = errors.New("perm: denied")

	// ErrSnapshotStale is returned by Resolve when no cached
	// snapshot is available and refresh failed past the
	// configured hard expiry.
	ErrSnapshotStale = errors.New("perm: snapshot stale; refresh failed past hard expiry")
)
