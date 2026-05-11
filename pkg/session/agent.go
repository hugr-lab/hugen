package session

import (
	"fmt"
	"strings"

	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// Agent is the acting principal in this process. Phase 1 has exactly
// one Agent per Runtime; later phases may extend this for sub-agents.
//
// Agent is a passive identity holder — Session is what runs the
// turn loop. Identity is fixed at construction.
//
// Phase 4.2.2 §9: the agent constitution splits in two:
//   - Universal preamble (every tier reads): tool naming, error
//     handling, general style.
//   - Per-tier section (only the matching tier reads): root /
//     mission / worker operating manuals. Selected via
//     ConstitutionFor(tier).
type Agent struct {
	id          string
	name        string
	src         identity.Source
	universal   string
	tierManuals map[string]string
}

// NewAgent constructs an Agent. id and name are the persisted
// identifiers; src is the underlying identity.Source. The
// `universal` body is the always-on preamble; `tierManuals` carries
// per-tier sections keyed by skill.TierRoot/Mission/Worker (missing
// keys collapse to "" — only the universal preamble appears for
// that tier). Phase 4.2.2 §9.
func NewAgent(id, name string, src identity.Source, universal string, tierManuals map[string]string) (*Agent, error) {
	if id == "" {
		return nil, fmt.Errorf("runtime: NewAgent requires id")
	}
	if src == nil {
		return nil, fmt.Errorf("runtime: NewAgent requires identity source")
	}
	if name == "" {
		name = "hugen"
	}
	manuals := make(map[string]string, len(tierManuals))
	for k, v := range tierManuals {
		manuals[k] = v
	}
	return &Agent{
		id:          id,
		name:        name,
		src:         src,
		universal:   universal,
		tierManuals: manuals,
	}, nil
}

func (a *Agent) ID() string   { return a.id }
func (a *Agent) Name() string { return a.name }

// Constitution returns the universal preamble — kept on the type
// for callers that don't know the calling session's tier (legacy
// adapter code, tests). systemPrompt uses ConstitutionFor instead.
func (a *Agent) Constitution() string { return a.universal }

// ConstitutionFor returns the system-prompt-ready constitution body
// the session at `tier` should see: universal preamble + the tier-
// specific manual. Empty entries are skipped gracefully so an agent
// with no per-tier manuals (legacy / test fixtures) still surfaces
// the universal preamble. Phase 4.2.2 §9.
func (a *Agent) ConstitutionFor(tier string) string {
	tier = strings.TrimSpace(tier)
	manual := a.tierManuals[tier]
	switch {
	case a.universal != "" && manual != "":
		return a.universal + "\n\n" + manual
	case manual != "":
		return manual
	default:
		return a.universal
	}
}

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
