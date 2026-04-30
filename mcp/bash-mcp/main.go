// Command bash-mcp is the in-tree file-and-shell MCP server,
// spawned by the runtime over stdio (per-agent lifetime). It
// exposes the bash.run, bash.shell, bash.read_file,
// bash.write_file, bash.list_dir, and bash.sed tools against a
// three-roots workspace: /workspace/<sid>/ (per-session,
// ephemeral), /shared/<aid>/ (agent-wide), and /readonly/<name>/
// (deployment mounts, read-only).
//
// Path resolution canonicalises via filepath.EvalSymlinks so
// symlink-escapes are caught at read/write time. Sandbox: 256
// MiB RSS, 30 s wall-clock default, output cap 32 KiB per stream.
// Permissions are enforced by the runtime's PermissionService
// before MCP dispatch — bash-mcp itself trusts the caller.
package main

import (
	"fmt"
	"os"
)

// Stubbed in T001. Real implementation arrives in T032.
func main() {
	fmt.Fprintln(os.Stderr, "bash-mcp: not yet implemented (phase-3 task T032)")
	os.Exit(2)
}
