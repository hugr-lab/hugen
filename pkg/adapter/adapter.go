// Package adapter is the seam between external channels and the
// runtime supervisor. Phase 1 ships pkg/adapter/console; later
// phases add SSE (phase 2) and a2a (phase 10).
//
// The Adapter and AdapterHost types live in pkg/runtime so the
// runtime can implement AdapterHost without an import cycle. This
// file is a thin shim that re-exports them for callers who want to
// import "pkg/adapter" rather than "pkg/runtime".
package adapter

import "github.com/hugr-lab/hugen/pkg/runtime"

// Adapter is an alias for runtime.Adapter — the interface every
// adapter implementation must satisfy.
type Adapter = runtime.Adapter

// Host is the adapter-side view of the runtime.
type Host = runtime.AdapterHost

// OpenRequest is the parameter shape for Host.OpenSession.
type OpenRequest = runtime.OpenRequest
