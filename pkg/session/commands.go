package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"sync"

	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// ErrCommandExists is returned by CommandRegistry.Register when a
// duplicate name is registered.
var ErrCommandExists = errors.New("runtime: command already registered")

// ErrCommandInvalidName is returned when Register is given a name
// that doesn't match the expected pattern.
var ErrCommandInvalidName = errors.New("runtime: invalid command name")

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// CommandHandler executes a slash command and returns Frames to be
// emitted onto the session's Outbox in order. Persistence is the
// caller's job (Session.Run); handlers stay pure with respect to
// I/O outside of CommandEnv.
type CommandHandler func(ctx context.Context, env CommandEnv, args []string) ([]protocol.Frame, error)

// CommandEnv carries the references a handler may need.
type CommandEnv struct {
	Session     *Session
	Author      protocol.ParticipantInfo
	AgentAuthor protocol.ParticipantInfo
	Models      *model.ModelRouter
	Notepad     *Notepad
	Logger      *slog.Logger
	// Description is set by Register for the /help listing.
	Description string
}

// CommandSpec is the registration shape: handler + one-line
// description for /help.
type CommandSpec struct {
	Handler     CommandHandler
	Description string
}

// CommandRegistry maps slash-command names to handlers.
type CommandRegistry struct {
	mu      sync.RWMutex
	entries map[string]CommandSpec
}

// NewCommandRegistry returns an empty registry. Built-in commands
// are registered separately by RegisterBuiltins.
func NewCommandRegistry() *CommandRegistry {
	return &CommandRegistry{entries: make(map[string]CommandSpec)}
}

// Register adds a command. Returns ErrCommandExists on duplicate.
func (r *CommandRegistry) Register(name string, spec CommandSpec) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("%w: %q", ErrCommandInvalidName, name)
	}
	if spec.Handler == nil {
		return fmt.Errorf("runtime: nil handler for command %q", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.entries[name]; dup {
		return fmt.Errorf("%w: %q", ErrCommandExists, name)
	}
	r.entries[name] = spec
	return nil
}

// Lookup returns the registered handler for name.
func (r *CommandRegistry) Lookup(name string) (CommandSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.entries[name]
	return s, ok
}

// Names returns all registered command names sorted lexically.
func (r *CommandRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.entries))
	for n := range r.entries {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Describe returns a multi-line "name — description" listing for
// every registered command. Used by the built-in /help.
func (r *CommandRegistry) Describe() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.entries))
	for n := range r.entries {
		names = append(names, n)
	}
	sort.Strings(names)
	var out string
	for _, n := range names {
		out += fmt.Sprintf("/%s — %s\n", n, r.entries[n].Description)
	}
	return out
}
