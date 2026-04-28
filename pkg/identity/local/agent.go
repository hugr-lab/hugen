package local

import (
	"context"
	"os"

	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/oasdiff/yaml"
)

// Agent retrieves the agent with the given id from hub
func (s *Source) Agent(ctx context.Context) (agent identity.Agent, err error) {
	if s.hub != nil {
		agent, err = s.hub.Agent(ctx)
		if err != nil {
			return identity.Agent{}, err
		}
	}
	if s.hub == nil {
		agent = identity.Agent{
			ID:          "local",
			AgentTypeID: "local",
			ShortID:     "local",
			Name:        "local",
			Status:      "active",
		}
	}

	if s.configPath != "" {
		agent.Config, err = loadLocalConfig(s.configPath)
		if err != nil {
			return identity.Agent{}, err
		}
	}

	return agent, nil
}

func loadLocalConfig(path string) (map[string]any, error) {
	f, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var config map[string]any
	if err := yaml.Unmarshal(f, &config); err != nil {
		return nil, err
	}

	return config, nil
}
