package httpapi

import "encoding/json"

// agentCard is the JSON served at /v1/agent — a static descriptor of this
// agent and the native HTTP protocol it speaks, analogous to the A2A card. A
// gateway / UI reads it to discover the agent's name, capabilities, and where
// to reach the API. Skill enumeration from the live skill set lands in a later
// step; H1 ships the identity + protocol descriptor.
type agentCard struct {
	Name         string       `json:"name"`
	Description  string       `json:"description"`
	Version      string       `json:"version"`
	API          apiDescriptor `json:"api"`
	Capabilities capabilities `json:"capabilities"`
	Skills       []skillRef   `json:"skills,omitempty"`
}

// apiDescriptor tells a client what protocol/transport to speak and where.
type apiDescriptor struct {
	Protocol  string `json:"protocol"`  // "hugen/v1"
	Transport string `json:"transport"` // "sse+http" — SSE reads, POST writes
	BaseURL   string `json:"base_url"`  // public base the client dials
	AuthScheme string `json:"auth_scheme"` // "bearer" — forwarded hub user token
}

// capabilities advertises the protocol features the agent supports. These
// describe the contract; the endpoints backing them land across H3–H6.
type capabilities struct {
	Streaming     bool `json:"streaming"`      // SSE frame stream (multi-subscriber)
	Inquiry       bool `json:"inquiry"`        // HITL input-required round-trip
	Artifacts     bool `json:"artifacts"`      // by-ref file artifacts
	AsyncMissions bool `json:"async_missions"` // long-running async sub-agents
}

type skillRef struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// marshalCard renders the agent card served at /v1/agent.
func (a *Adapter) marshalCard() ([]byte, error) {
	return json.Marshal(agentCard{
		Name:        a.agentName,
		Description: a.agentDesc,
		Version:     agentVersion,
		API: apiDescriptor{
			Protocol:   "hugen/v1",
			Transport:  "sse+http",
			BaseURL:    a.baseURL,
			AuthScheme: "bearer",
		},
		Capabilities: capabilities{
			Streaming:     true,
			Inquiry:       true,
			Artifacts:     true,
			AsyncMissions: true,
		},
	})
}
