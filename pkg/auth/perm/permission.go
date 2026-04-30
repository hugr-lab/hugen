package perm

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/hugr-lab/hugen/pkg/auth/template"
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

// Identity is the caller identity threaded through Resolve. The
// fields used by [pkg/auth/template.Apply] mirror Context one for
// one.
type Identity struct {
	UserID          string
	AgentID         string
	Role            string
	Roles           []string
	SessionID       string
	SessionMetadata map[string]string
}

// TemplateContext converts an Identity to the template package's
// Context type. Convenience for callers that need to substitute
// before dispatch.
func (i Identity) TemplateContext() template.Context {
	return template.Context{
		UserID:          i.UserID,
		Role:            i.Role,
		AgentID:         i.AgentID,
		SessionID:       i.SessionID,
		SessionMetadata: i.SessionMetadata,
	}
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
