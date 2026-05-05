// Package recovery provides a per-call retry decorator for
// tool.ToolProvider instances whose underlying resource can be
// re-established after failure.
//
// Phase 4.1a stage A step 6b introduces this subpackage to
// replace the legacy ToolManager-side Reconnector goroutine
// loop. The new model is lazy:
//
//   - Wrap any ToolProvider with recovery.Wrap. The wrapper
//     forwards Name / Lifetime / Subscribe / Close, and decorates
//     List + Call with retry behaviour.
//   - On failure the wrapper checks whether the inner provider
//     implements tool.Recoverable. Non-Recoverable providers
//     surface the error verbatim (no retry).
//   - For Recoverable providers, the wrapper walks a backoff
//     schedule (default 5s / 10s / 30s), calling TryReconnect
//     between attempts. The first successful retry returns;
//     exhaustion returns the last error.
//
// Recovery is composable — system tools that don't benefit from
// retry (admin, policies, session.Manager) get registered without
// the wrapper; flaky transports (mcp) get wrapped at construction
// time inside pkg/runtime.
//
// Imports: pkg/tool only.
package recovery
