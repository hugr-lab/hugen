package store

import "time"

// AgentType describes one kind of agent (e.g. "hugr-data") and its
// default configuration. Multiple agents may share the same type.
type AgentType struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Config      map[string]any `json:"config"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

// Agent is a running agent instance.
type Agent struct {
	ID             string         `json:"id"`
	AgentTypeID    string         `json:"agent_type_id"`
	ShortID        string         `json:"short_id"`
	Name           string         `json:"name"`
	Status         string         `json:"status"`
	ConfigOverride map[string]any `json:"config_override"`
	CreatedAt      time.Time      `json:"created_at"`
	LastActive     time.Time      `json:"last_active"`
}
