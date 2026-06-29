// Package adapter is the seam between external channels and the
// runtime supervisor. pkg/adapter/tui hosts the Bubble Tea
// interactive renderer — currently the only adapter. Future surfaces
// (design/008): an A2A adapter (Teams/Copilot interop) and a hub
// HTTP web-app adapter — the planned richest client, likely with more
// features than TUI/A2A. An earlier SSE/Last-Event-ID HTTP adapter +
// webui SPA were removed as dead code; their source survives in git
// history as a starting reference for the hub web app.
//
// Adapter and AdapterHost live in pkg/session/manager (the
// supervisor); OpenRequest stays in pkg/session because Session
// constructors consume it. This file is a thin shim that re-exports
// these types for callers who want to import "pkg/adapter" rather
// than reach into the runtime/session split.
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
