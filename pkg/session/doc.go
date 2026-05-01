// Package runtime is the core of the hugen agent.
//
// It owns the Runtime supervisor (one per process), the
// Manager (live *Session map), the Agent (the acting
// principal), and the RuntimeStore (persistence facade). Adapters
// — console, sse (phase 2), a2a (phase 10) — interact with the
// runtime through the Adapter interface declared in
// runtime.go (also re-exported as pkg/adapter.Adapter).
//
// See design/001-agent-runtime/design.md for the full
// architectural picture.
package session
