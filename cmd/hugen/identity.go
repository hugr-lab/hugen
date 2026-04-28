package main

import (
	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/identity/hub"
	"github.com/hugr-lab/hugen/pkg/identity/local"
	"github.com/hugr-lab/query-engine/types"
)

func buildIdentity(boot *BootstrapConfig, remote types.Querier) identity.Source {
	switch {
	case boot.IsRemoteMode():
		return hub.New(remote)
	case boot.IsLocalMode() && remote == nil:
		return local.New(boot.ConfigPath)
	default:
		return local.NewWithHub(hub.New(remote), boot.ConfigPath)
	}
}
