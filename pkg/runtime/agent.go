package runtime

import (
	"fmt"

	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// Agent is the acting principal in this process. Phase 1 has exactly
// one Agent per Runtime; later phases may extend this for sub-agents.
//
// Agent is a passive identity holder — Session is what runs the
// turn loop. Identity is fixed at construction.
type Agent struct {
	id   string
	name string
	src  identity.Source
}

// NewAgent constructs an Agent. id and name are the persisted
// identifiers; src is the underlying identity.Source (which may be
// re-queried at runtime if needed, e.g. for permission checks).
func NewAgent(id, name string, src identity.Source) (*Agent, error) {
	if id == "" {
		return nil, fmt.Errorf("runtime: NewAgent requires id")
	}
	if src == nil {
		return nil, fmt.Errorf("runtime: NewAgent requires identity source")
	}
	if name == "" {
		name = "hugen"
	}
	return &Agent{id: id, name: name, src: src}, nil
}

func (a *Agent) ID() string                      { return a.id }
func (a *Agent) Name() string                    { return a.name }
func (a *Agent) IdentitySource() identity.Source { return a.src }

// Participant returns the agent's ParticipantInfo for use in Frame
// authorship.
func (a *Agent) Participant() protocol.ParticipantInfo {
	return protocol.ParticipantInfo{
		ID:   a.id,
		Kind: protocol.ParticipantAgent,
		Name: a.name,
	}
}
