// Package adapter is the seam between external channels and the
// runtime supervisor. Phase 1 ships pkg/adapter/console; later
// phases add SSE (phase 2) and a2a (phase 10).
//
// Adapter and AdapterHost live in pkg/session/manager (the
// supervisor); OpenRequest stays in pkg/session because Session
// constructors consume it. This file is a thin shim that re-exports
// the trio for callers who want to import "pkg/adapter" rather than
// reach into the runtime/session split.
package adapter

import (
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/session/manager"
)

// Adapter is an alias for manager.Adapter — the interface every
// adapter implementation must satisfy.
type Adapter = manager.Adapter

// Host is the adapter-side view of the session.
type Host = manager.AdapterHost

// OpenRequest is the parameter shape for Host.OpenSession.
type OpenRequest = session.OpenRequest
