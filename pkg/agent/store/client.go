// Package store exposes hub.db agent + agent_types operations
// through a typed Client.
package store

import (
	"fmt"
	"log/slog"

	"github.com/hugr-lab/query-engine/types"
)

// Options configures the Client.
type Options struct {
	AgentID    string
	AgentShort string
	Logger     *slog.Logger
}

// Client is the hub.db agent-registry API.
type Client struct {
	querier    types.Querier
	agentID    string
	agentShort string
	logger     *slog.Logger
}

// New constructs the Client.
func New(querier types.Querier, opts Options) (*Client, error) {
	if querier == nil {
		return nil, fmt.Errorf("registry: nil querier")
	}
	if opts.AgentID == "" {
		return nil, fmt.Errorf("registry: AgentID required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Client{
		querier:    querier,
		agentID:    opts.AgentID,
		agentShort: opts.AgentShort,
		logger:     opts.Logger,
	}, nil
}

// AgentID returns the scope the Client was constructed for.
func (c *Client) AgentID() string { return c.agentID }
