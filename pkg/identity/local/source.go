package local

import (
	"github.com/hugr-lab/hugen/pkg/identity/hub"
)

type Source struct {
	hub *hub.Source

	configPath string
}

func New(configPath string) *Source {
	return &Source{configPath: configPath}
}

func NewWithHub(hub *hub.Source, configPath string) *Source {
	return &Source{hub: hub, configPath: configPath}
}
