package identity

import (
	"context"
	"time"
)

type Source interface {
	Agent(ctx context.Context) (Agent, error)
	WhoAmI(ctx context.Context) (WhoAmI, error)
	Permission(ctx context.Context, section, name string) (Permission, error)
}

// Agent is a running agent instance.
type Agent struct {
	ID          string         `json:"id"`
	AgentTypeID string         `json:"agent_type_id"`
	Type        string         `json:"type"`
	ShortID     string         `json:"short_id"`
	Name        string         `json:"name"`
	Status      string         `json:"status"`
	Config      map[string]any `json:"config"`
	CreatedAt   time.Time      `json:"created_at"`
	LastActive  time.Time      `json:"last_active"`
}

// WhoAmI is the minimal subject description returned by the hugr
// auth.me endpoint. The hugr client has already applied the bearer
// token via its transport, so the query resolves against whatever
// principal the token represents.
type WhoAmI struct {
	UserID   string `json:"user_id"`
	UserName string `json:"user_name"`
	Role     string `json:"role"`
}

type Permission struct {
	Enabled bool           `json:"enabled"`
	Data    map[string]any `json:"data"`
	Filters map[string]any `json:"filters"`
}
