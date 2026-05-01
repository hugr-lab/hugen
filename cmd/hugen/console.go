package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/hugr-lab/hugen/pkg/adapter/console"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
)

// runConsole attaches the console adapter to a shared *RuntimeCore
// and runs the runtime until ctx cancels. Per
// contracts/runtime-core.md, the handler MUST NOT re-bootstrap any
// dependency already produced by buildRuntimeCore.
func runConsole(ctx context.Context, core *RuntimeCore) int {
	resumeID := tryFindResumableSession(ctx, core.Manager, core.Logger)

	consoleAdapter := console.New(
		console.WithLogger(core.Logger),
		consoleResumeOption(resumeID),
		console.WithUser(operatorParticipant()),
	)

	rt := session.NewRuntime(core.Manager, []session.Adapter{consoleAdapter}, core.Logger)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = rt.Shutdown(shutdownCtx)
	}()

	if err := rt.Start(ctx); err != nil {
		if ctx.Err() != nil {
			core.Logger.Info("shutdown complete")
			return exitOK
		}
		core.Logger.Error("runtime exited", "err", err)
		return 1
	}
	return exitOK
}

func tryFindResumableSession(ctx context.Context, m *session.Manager, logger *slog.Logger) string {
	rows, err := m.List(ctx, session.StatusActive)
	if err != nil {
		logger.Warn("list active sessions", "err", err)
		return ""
	}
	if len(rows) == 0 {
		return ""
	}
	logger.Info("resumable sessions found", "count", len(rows), "resuming", rows[0].ID)
	return rows[0].ID
}

func consoleResumeOption(id string) console.Option {
	if id == "" {
		return func(*console.Adapter) {}
	}
	return console.WithResumeSession(id)
}

func operatorParticipant() protocol.ParticipantInfo {
	id := os.Getenv("USER")
	if id == "" {
		id = "operator"
	}
	return protocol.ParticipantInfo{ID: id, Kind: protocol.ParticipantUser, Name: id}
}

// registerBuiltinCommands wires the Phase-1 set of slash commands
// onto the registry. Phase 5 adds /help, /note, /cancel, /end,
// /model bodies; this registration is the seam.
func registerBuiltinCommands(reg *session.CommandRegistry, logger *slog.Logger) error {
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
		id, err := env.Notepad.Append(ctx, env.Author.ID, text)
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
		// The actual stream-stop happens in Session.handleCancel; we
		// just emit the Cancel frame so the transcript records intent.
		reason := "user_cancelled"
		if len(args) > 0 {
			reason = joinArgs(args)
		}
		return []protocol.Frame{
			protocol.NewCancel(env.Session.ID(), env.Author, reason),
		}, nil
	}
}

func endHandler() session.CommandHandler {
	return func(ctx context.Context, env session.CommandEnv, args []string) ([]protocol.Frame, error) {
		reason := "user_end"
		if len(args) > 0 {
			reason = joinArgs(args)
		}
		// The dispatcher in session.handleSlashCommand recognises a
		// returned SessionClosed and runs MarkClosed after emitting
		// it. The handler stays pure.
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
	// First, try as an intent name.
	if spec, ok := r.SpecFor(model.Intent(target)); ok {
		return spec, nil
	}
	// Then as a provider/name spec.
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
