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
		{"cancel_subagent", "cancel a running child mission: /cancel_subagent <session_id> [reason]", cancelSubagentHandler()},
		{"cancel_all_subagents", "cancel every running child mission: /cancel_all_subagents [reason]", cancelAllSubagentsHandler()},
		{"dismiss_subagent", "acknowledge a parked child and tear it down: /dismiss_subagent <session_id>", dismissSubagentHandler()},
		{"notify_subagent", "deliver a directive to a child (re-arm if parked): /notify_subagent <session_id> <text...>", notifySubagentHandler()},
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

// cancelSubagentHandler implements `/cancel_subagent <id> [reason]`.
// The operator-side counterpart of the model-callable
// `session:subagent_cancel` tool. Phase 5.1c.cancel-ux.
func cancelSubagentHandler() session.CommandHandler {
	return func(ctx context.Context, env session.CommandEnv, args []string) ([]protocol.Frame, error) {
		if len(args) < 1 {
			return []protocol.Frame{
				protocol.NewError(env.Session.ID(), env.AgentAuthor, "usage_error",
					"usage: /cancel_subagent <session_id> [reason]", false),
			}, nil
		}
		childID := args[0]
		// Phase 5.x.skill-polish-1 (R7) — distinct default label
		// from the model-callable `session:subagent_cancel` tool
		// (which uses "user_request") so event-log forensics can
		// tell apart "operator hit Esc" from "model decided to
		// cancel". Operators almost always pass a real reason via
		// the `/mission` modal; this default only fires for bare
		// CLI typing like `/cancel_subagent ses-abc`.
		reason := "slash_command"
		if len(args) > 1 {
			reason = joinArgs(args[1:])
		}
		cancelled, err := env.Session.RequestChildCancel(ctx, childID, reason)
		if err != nil {
			return []protocol.Frame{
				protocol.NewError(env.Session.ID(), env.AgentAuthor, "cancel_failed", err.Error(), false),
			}, nil
		}
		// Phase 5.1c.cancel-ux follow-up — fail loudly on an
		// operator typo. RequestChildCancel returns cancelled=false
		// when the id is not in the live children map; without
		// this branch a typo gets silently confirmed.
		if !cancelled {
			return []protocol.Frame{
				protocol.NewError(env.Session.ID(), env.AgentAuthor, "no_such_session",
					fmt.Sprintf("no live child mission with id %q (already completed or typo?)", childID), false),
			}, nil
		}
		return []protocol.Frame{
			protocol.NewSystemMarker(env.Session.ID(), env.AgentAuthor, "subagent_cancel_requested",
				map[string]any{"session_id": childID, "reason": reason}),
		}, nil
	}
}

// dismissSubagentHandler implements `/dismiss_subagent <id>`. The
// operator-side counterpart of the model-callable
// `session:subagent_dismiss` tool. Phase 5.2 subagent-lifetime γ.
//
// Strict on lifecycle state: a child that isn't parked returns
// `not_parked` so the operator does not accidentally use dismiss
// in place of cancel.
func dismissSubagentHandler() session.CommandHandler {
	return func(ctx context.Context, env session.CommandEnv, args []string) ([]protocol.Frame, error) {
		if len(args) < 1 {
			return []protocol.Frame{
				protocol.NewError(env.Session.ID(), env.AgentAuthor, "usage_error",
					"usage: /dismiss_subagent <session_id>", false),
			}, nil
		}
		childID := args[0]
		ok, err := env.Session.RequestChildDismiss(ctx, childID)
		if err != nil {
			code := "dismiss_failed"
			switch {
			case errors.Is(err, session.ErrCancelEmptyID):
				code = "usage_error"
			case errors.Is(err, session.ErrChildNotParked):
				code = "not_parked"
			}
			return []protocol.Frame{
				protocol.NewError(env.Session.ID(), env.AgentAuthor, code, err.Error(), false),
			}, nil
		}
		if !ok {
			return []protocol.Frame{
				protocol.NewError(env.Session.ID(), env.AgentAuthor, "no_such_session",
					fmt.Sprintf("no live child mission with id %q (already dismissed or typo?)", childID), false),
			}, nil
		}
		return []protocol.Frame{
			protocol.NewSystemMarker(env.Session.ID(), env.AgentAuthor, "subagent_dismiss_requested",
				map[string]any{"session_id": childID}),
		}, nil
	}
}

// notifySubagentHandler implements `/notify_subagent <id> <text...>`.
// The operator-side counterpart of the model-callable
// `session:notify_subagent` tool. Behaviour follows the tool: parked
// children get a re-arm UserMessage, active children get a
// SystemMessage parent_note. Phase 5.2 subagent-lifetime γ.
func notifySubagentHandler() session.CommandHandler {
	return func(ctx context.Context, env session.CommandEnv, args []string) ([]protocol.Frame, error) {
		if len(args) < 2 {
			return []protocol.Frame{
				protocol.NewError(env.Session.ID(), env.AgentAuthor, "usage_error",
					"usage: /notify_subagent <session_id> <text...>", false),
			}, nil
		}
		childID := args[0]
		content := joinArgs(args[1:])
		rearmed, err := env.Session.RequestChildNotify(ctx, childID, content)
		if err != nil {
			return []protocol.Frame{
				protocol.NewError(env.Session.ID(), env.AgentAuthor, "notify_failed", err.Error(), false),
			}, nil
		}
		return []protocol.Frame{
			protocol.NewSystemMarker(env.Session.ID(), env.AgentAuthor, "subagent_notify_requested",
				map[string]any{"session_id": childID, "rearmed": rearmed}),
		}, nil
	}
}

// cancelAllSubagentsHandler implements `/cancel_all_subagents [reason]`.
// Fans out RequestChildCancel against every direct child. Phase
// 5.1c.cancel-ux — used by the `/mission` modal's Shift+C action and
// the Esc-Esc panic-cancel gesture.
func cancelAllSubagentsHandler() session.CommandHandler {
	return func(ctx context.Context, env session.CommandEnv, args []string) ([]protocol.Frame, error) {
		reason := "slash_command_all"
		if len(args) > 0 {
			reason = joinArgs(args)
		}
		ids := env.Session.RequestAllChildrenCancel(ctx, reason)
		return []protocol.Frame{
			protocol.NewSystemMarker(env.Session.ID(), env.AgentAuthor, "subagent_cancel_all_requested",
				map[string]any{"session_ids": ids, "count": len(ids), "reason": reason}),
		}, nil
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
