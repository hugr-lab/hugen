package runtime

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/hugr-lab/hugen/assets"
	notepadext "github.com/hugr-lab/hugen/pkg/extension/notepad"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
)

// constitutionSubdir is the directory under StateDir where the
// agent constitution materialises. Operators override the bundled
// copy by editing files in this directory; if a file exists at
// boot, it shadows the embedded one.
const constitutionSubdir = "constitution"

// constitutionDefaultFile is the universal-rules markdown.
const constitutionDefaultFile = "agent.md"

// LoadConstitution returns the agent's constitution markdown body.
// Search order:
//  1. ${stateDir}/constitution/agent.md — operator override.
//  2. assets/constitution/agent.md — bundled default.
//
// On first boot the bundled copy is also materialised at
// ${stateDir}/constitution/agent.md so the operator has a starting
// point to edit. Updating the binary refreshes the on-disk copy
// only when the operator hasn't customised it (file matches
// embedded byte-for-byte) — otherwise the operator's edits stay.
func LoadConstitution(stateDir string, log *slog.Logger) (string, error) {
	if stateDir == "" {
		return "", errors.New("constitution: empty state dir")
	}
	target := filepath.Join(stateDir, constitutionSubdir)
	if err := os.MkdirAll(target, 0o755); err != nil {
		return "", fmt.Errorf("constitution: mkdir: %w", err)
	}

	embedded, err := fs.ReadFile(assets.ConstitutionFS, filepath.Join("constitution", constitutionDefaultFile))
	if err != nil {
		return "", fmt.Errorf("constitution: read embed: %w", err)
	}

	disk := filepath.Join(target, constitutionDefaultFile)
	current, err := os.ReadFile(disk)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.WriteFile(disk, embedded, 0o644); err != nil {
			return "", fmt.Errorf("constitution: write default: %w", err)
		}
		log.Info("constitution materialised", "path", disk)
		return string(embedded), nil
	case err != nil:
		return "", fmt.Errorf("constitution: read disk: %w", err)
	}

	// Operator may have edited the on-disk copy — preserve it.
	// Treat operator's bytes as authoritative; the embedded copy
	// is only a starting template.
	return string(current), nil
}

// RegisterBuiltinCommands wires the Phase-1 set of slash commands
// onto the registry: /help, /note, /cancel, /end, /model.
func RegisterBuiltinCommands(reg *session.CommandRegistry, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	binds := []struct {
		name        string
		description string
		handler     session.CommandHandler
	}{
		{"help", "list available commands", helpHandler(reg)},
		{"note", "save a note to the session notepad: /note <text>", noteHandler()},
		{"cancel", "cancel the in-flight turn", cancelHandler()},
		{"end", "close the current session", endHandler()},
		{"model", "switch the model for this session: /model use <intent|provider/name>", modelHandler()},
	}
	for _, b := range binds {
		if err := reg.Register(b.name, session.CommandSpec{
			Handler:     b.handler,
			Description: b.description,
		}); err != nil {
			return fmt.Errorf("register /%s: %w", b.name, err)
		}
	}
	logger.Debug("commands registered", "count", len(binds))
	return nil
}

func helpHandler(reg *session.CommandRegistry) session.CommandHandler {
	return func(ctx context.Context, env session.CommandEnv, args []string) ([]protocol.Frame, error) {
		body := "Available commands:\n" + reg.Describe()
		return []protocol.Frame{
			protocol.NewAgentMessage(env.Session.ID(), env.AgentAuthor, body, 0, true),
		}, nil
	}
}

func noteHandler() session.CommandHandler {
	return func(ctx context.Context, env session.CommandEnv, args []string) ([]protocol.Frame, error) {
		if len(args) == 0 {
			return []protocol.Frame{
				protocol.NewError(env.Session.ID(), env.AgentAuthor, "empty_note",
					"usage: /note <text>", false),
			}, nil
		}
		text := joinArgs(args)
		np := notepadext.FromState(env.Session)
		if np == nil {
			return []protocol.Frame{
				protocol.NewError(env.Session.ID(), env.AgentAuthor, "note_failed",
					"notepad extension not registered on this session", false),
			}, nil
		}
		id, err := np.Append(ctx, env.Author.ID, text)
		if err != nil {
			return []protocol.Frame{
				protocol.NewError(env.Session.ID(), env.AgentAuthor, "note_failed", err.Error(), true),
			}, nil
		}
		return []protocol.Frame{
			protocol.NewSystemMarker(env.Session.ID(), env.AgentAuthor, "note_added",
				map[string]any{"note_id": id}),
		}, nil
	}
}

func cancelHandler() session.CommandHandler {
	return func(ctx context.Context, env session.CommandEnv, args []string) ([]protocol.Frame, error) {
		cascade := false
		if len(args) > 0 && args[0] == "all" {
			cascade = true
			args = args[1:]
		}
		reason := "user_cancelled"
		if len(args) > 0 {
			reason = joinArgs(args)
		}
		c := protocol.NewCancel(env.Session.ID(), env.Author, reason)
		c.Payload.Cascade = cascade
		return []protocol.Frame{c}, nil
	}
}

func endHandler() session.CommandHandler {
	return func(ctx context.Context, env session.CommandEnv, args []string) ([]protocol.Frame, error) {
		reason := "user_end"
		if len(args) > 0 {
			reason = joinArgs(args)
		}
		closed := protocol.NewSessionClosed(env.Session.ID(), env.AgentAuthor, reason)
		return []protocol.Frame{closed}, nil
	}
}

func modelHandler() session.CommandHandler {
	return func(ctx context.Context, env session.CommandEnv, args []string) ([]protocol.Frame, error) {
		if len(args) < 2 || args[0] != "use" {
			return []protocol.Frame{
				protocol.NewError(env.Session.ID(), env.AgentAuthor, "usage_error",
					"usage: /model use <intent|provider/name>", false),
			}, nil
		}
		target := args[1]
		spec, err := resolveModelTarget(env.Models, target)
		if err != nil {
			return []protocol.Frame{
				protocol.NewError(env.Session.ID(), env.AgentAuthor, "unknown_model", err.Error(), false),
			}, nil
		}
		env.Session.SetModelOverride(model.IntentDefault, spec)
		return []protocol.Frame{
			protocol.NewSystemMarker(env.Session.ID(), env.AgentAuthor, "model_switched_pending",
				map[string]any{"to": spec.String()}),
		}, nil
	}
}

func resolveModelTarget(r *model.ModelRouter, target string) (model.ModelSpec, error) {
	if spec, ok := r.SpecFor(model.Intent(target)); ok {
		return spec, nil
	}
	for _, sep := range []string{"/", ":"} {
		if i := indexByte(target, sep[0]); i > 0 {
			spec := model.ModelSpec{Provider: target[:i], Name: target[i+1:]}
			if r.Has(spec) {
				return spec, nil
			}
		}
	}
	return model.ModelSpec{}, fmt.Errorf("no model registered for %q (try one of: %s)", target, knownIntents(r))
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func knownIntents(r *model.ModelRouter) string {
	defaults := r.Defaults()
	out := ""
	first := true
	for intent := range defaults {
		if !first {
			out += ", "
		}
		out += string(intent)
		first = false
	}
	return out
}

func joinArgs(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}

// phaseAgent runs phase 6: loads the constitution, builds the
// session.Agent identity, registers the Phase-1 builtin slash
// commands, and constructs the protocol.Codec. Populates Core.Agent,
// Core.Commands, Core.Codec.
func phaseAgent(ctx context.Context, core *Core) error {
	agentInfo, err := core.Identity.Agent(ctx)
	if err != nil {
		return fmt.Errorf("identity: %w", err)
	}
	constitution, err := LoadConstitution(core.Cfg.StateDir, core.Logger)
	if err != nil {
		return fmt.Errorf("constitution: %w", err)
	}
	agent, err := session.NewAgent(agentInfo.ID, agentInfo.Name, core.Identity, constitution)
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	core.Agent = agent

	cmds := session.NewCommandRegistry()
	if err := RegisterBuiltinCommands(cmds, core.Logger); err != nil {
		return fmt.Errorf("commands: %w", err)
	}
	core.Commands = cmds

	core.Codec = protocol.NewCodec()
	return nil
}
