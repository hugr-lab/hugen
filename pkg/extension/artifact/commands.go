package artifact

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// Compile-time assertion: artifact contributes user-typed slash
// commands so the runtime registers them on every session.
var _ extension.Commander = (*Extension)(nil)

// Commands implements [extension.Commander]:
//   - /artifacts        list this conversation's artifacts (+ on-disk paths)
//   - /attach <path>    ingest a local host file as an artifact (upload)
//   - /open <name>      OS-open a published artifact (local download)
func (e *Extension) Commands() []extension.Command {
	return []extension.Command{
		{Name: "artifacts", Description: "list this conversation's artifacts: /artifacts", Handler: e.cmdArtifacts},
		{Name: "attach", Description: "attach a local file as an artifact: /attach <path>", Handler: e.cmdAttach},
		{Name: "open", Description: "open a published artifact: /open <name>", Handler: e.cmdOpen},
	}
}

func (e *Extension) cmdArtifacts(_ context.Context, state extension.SessionState, env extension.CommandContext, _ []string) ([]protocol.Frame, error) {
	sid := state.SessionID()
	sa := FromState(state)
	if sa == nil {
		return errFrame(sid, env, "artifacts extension not on this session"), nil
	}
	refs, err := e.store.List(sa.rootID)
	if err != nil {
		return errFrame(sid, env, "list failed: "+err.Error()), nil
	}
	if len(refs) == 0 {
		return msgFrame(sid, env, "artifacts_list", "No artifacts in this conversation yet."), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Artifacts (%d):\n", len(refs))
	for _, r := range refs {
		path, _ := e.store.Path(sa.rootID, r.ID)
		fmt.Fprintf(&b, "- %s  (%s, %s)\n  %s\n", r.ID, mimeShort(r.MIME), humanSize(r.Size), path)
	}
	b.WriteString("\nOpen one with /open <name>.")
	return msgFrame(sid, env, "artifacts_list", strings.TrimRight(b.String(), "\n")), nil
}

func (e *Extension) cmdAttach(_ context.Context, state extension.SessionState, env extension.CommandContext, args []string) ([]protocol.Frame, error) {
	sid := state.SessionID()
	if len(args) == 0 {
		return errFrame(sid, env, "usage: /attach <path> (absolute, ~/…, $VAR/…, or relative to the hugen launch dir)"), nil
	}
	sa := FromState(state)
	if sa == nil {
		return errFrame(sid, env, "artifacts extension not on this session"), nil
	}
	// Expand a leading ~ / $VAR BEFORE resolving — filepath.Abs treats a
	// literal "~" as a path segment joined onto the process cwd, so a
	// `/attach ~/x` would otherwise resolve to a bogus <cwd>/~/x. After
	// expansion, Abs makes a relative path absolute against the hugen
	// process cwd (the launch dir).
	src := expandPath(strings.Join(args, " "))
	if abs, err := filepath.Abs(src); err == nil {
		src = abs
	}
	ref, err := e.Ingest(sa.rootID, src, "")
	if err != nil {
		return errFrame(sid, env, "attach failed: "+err.Error()), nil
	}
	// Announce the upload (refs only) + a user-visible confirmation.
	data, _ := json.Marshal(ref)
	uploaded := protocol.NewExtensionFrame(sid, extension.AgentParticipant(e.agentID),
		providerName, protocol.CategoryMarker, OpUploaded, data)
	body := fmt.Sprintf("Attached `%s` as artifact `%s` (%s). The agent can read it with artifact:copy.",
		filepath.Base(src), ref.ID, humanSize(ref.Size))
	return []protocol.Frame{uploaded, protocol.NewSystemMessage(sid, env.AgentAuthor, "artifact_attached", body)}, nil
}

func (e *Extension) cmdOpen(_ context.Context, state extension.SessionState, env extension.CommandContext, args []string) ([]protocol.Frame, error) {
	sid := state.SessionID()
	if len(args) == 0 {
		return errFrame(sid, env, "usage: /open <name>"), nil
	}
	sa := FromState(state)
	if sa == nil {
		return errFrame(sid, env, "artifacts extension not on this session"), nil
	}
	id := args[0]
	path, err := e.store.Path(sa.rootID, id)
	if err != nil {
		return errFrame(sid, env, fmt.Sprintf("no artifact %q (see /artifacts)", id)), nil
	}
	// OS-open on the host. Local mode: the runtime IS the user's host,
	// so this launches their default app. On failure (headless / no
	// opener) fall back to printing the path. Start (not Run) so we
	// never block the command path on a GUI app.
	if oerr := openOnHost(path); oerr != nil {
		return msgFrame(sid, env, "artifact_open",
			fmt.Sprintf("Could not auto-open (%v). The file is at:\n%s", oerr, path)), nil
	}
	return msgFrame(sid, env, "artifact_open", fmt.Sprintf("Opening `%s` …\n%s", id, path)), nil
}

// openOnHost launches the platform's default opener for path. Start,
// not Run — the opener detaches and we don't wait on the GUI app.
func openOnHost(path string) error {
	var argv []string
	switch runtime.GOOS {
	case "darwin":
		argv = []string{"open", path}
	case "windows":
		argv = []string{"cmd", "/c", "start", "", path}
	default:
		argv = []string{"xdg-open", path}
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	return cmd.Start()
}

// expandPath resolves a leading ~ / ~/ to the user's home directory
// and $VAR / ${VAR} via the environment, so /attach accepts an
// absolute path, ~/…, $VAR/…, or a relative path. Mirrors bash-mcp's
// expandPath. Only a LEADING ~ is special (mid-path ~ is a legal
// filename character); an unset env var expands to "" (os.ExpandEnv
// semantics).
func expandPath(input string) string {
	s := os.ExpandEnv(input)
	switch {
	case s == "~":
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	case strings.HasPrefix(s, "~/"):
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, s[2:])
		}
	}
	return s
}

// ---- small frame + format helpers ----

func msgFrame(sid string, env extension.CommandContext, kind, body string) []protocol.Frame {
	return []protocol.Frame{protocol.NewSystemMessage(sid, env.AgentAuthor, kind, body)}
}

func errFrame(sid string, env extension.CommandContext, msg string) []protocol.Frame {
	return []protocol.Frame{protocol.NewError(sid, env.AgentAuthor, "artifact_command", msg, false)}
}

// mimeShort trims the "; charset=…" suffix for a compact listing.
func mimeShort(m string) string {
	if i := strings.IndexByte(m, ';'); i >= 0 {
		return m[:i]
	}
	if m == "" {
		return "?"
	}
	return m
}

// humanSize renders a byte count as B / KB / MB.
func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
