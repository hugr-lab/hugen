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
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
	skillpkg "github.com/hugr-lab/hugen/pkg/skill"
)

// constitutionDefaultFile is the universal-rules markdown — the
// shared preamble every tier reads.
const constitutionDefaultFile = "agent.md"

// constitutionEmbedRoot is the directory inside assets.ConstitutionFS
// that holds the bundled files.
const constitutionEmbedRoot = "constitution"

// constitutionTierFiles maps each session tier to the manual the
// runtime concatenates with the universal preamble. Each tier's
// manual is a self-contained operating instruction set for sessions
// at that tier. Phase 4.2.2 §9.
var constitutionTierFiles = map[string]string{
	skillpkg.TierRoot:    "tier-root.md",
	skillpkg.TierMission: "tier-mission.md",
	skillpkg.TierWorker:  "tier-worker.md",
}

// LoadConstitution returns the agent's universal constitution
// markdown body, read straight from the embedded
// assets.ConstitutionFS.
//
// Constitution is core agent behaviour wired into the binary —
// not tunable by operators, not materialised to disk. Binary
// upgrades flow through automatically; there is no on-disk shadow
// to go stale. Phase 5.1 refresh-fix 2026-05-13.
func LoadConstitution(_ *slog.Logger) (string, error) {
	body, _, err := loadConstitutionBundle()
	return body, err
}

// loadConstitutionBundle returns the universal preamble + the
// per-tier manuals map, all read from the embedded
// assets.ConstitutionFS. Missing tier manuals are tolerated
// (skipped) so a stripped-down bundle can carry only the
// universal preamble.
func loadConstitutionBundle() (string, map[string]string, error) {
	universal, err := readConstitutionEmbed(constitutionDefaultFile)
	if err != nil {
		return "", nil, fmt.Errorf("constitution: %s: %w", constitutionDefaultFile, err)
	}
	manuals := make(map[string]string, len(constitutionTierFiles))
	for tier, file := range constitutionTierFiles {
		body, err := readConstitutionEmbed(file)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// Tier file is optional — skip silently.
			continue
		case err != nil:
			return "", nil, fmt.Errorf("constitution: %s: %w", file, err)
		}
		manuals[tier] = body
	}
	return universal, manuals, nil
}

// readConstitutionEmbed reads one file from the embedded bundle.
// Returns fs.ErrNotExist (unwrapped) when the name is not in the
// bundle so callers can detect optional tier files cleanly.
func readConstitutionEmbed(name string) (string, error) {
	body, err := fs.ReadFile(assets.ConstitutionFS, filepath.Join(constitutionEmbedRoot, name))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", fs.ErrNotExist
	case err != nil:
		return "", fmt.Errorf("read embed: %w", err)
	}
	return string(body), nil
}

// RegisterBuiltinCommands wires the session-core slash commands onto
// the registry: /help, /cancel, /end, /model. Extension-owned
// commands (e.g. /note from notepad, /skill from skill) are
// registered separately by phaseExtensions via the
// [extension.Commander] capability.
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
	universal, tierManuals, err := loadConstitutionBundle()
	if err != nil {
		return fmt.Errorf("constitution: %w", err)
	}
	agent, err := session.NewAgent(agentInfo.ID, agentInfo.Name, core.Identity, universal, tierManuals)
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
