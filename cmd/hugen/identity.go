package main

import (
	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/identity/hub"
	"github.com/hugr-lab/hugen/pkg/identity/local"
	"github.com/hugr-lab/query-engine/types"
)

// buildIdentity selects the identity source for the configured mode:
//
//   - remote (personal-assistant): the agent acts as a hub user, so
//     identity flows from the remote hub through hub.New.
//   - local  (autonomous-agent):   the agent has no hub; identity is
//     read from the local config.yaml.
//
// The previous "local-with-hub" hybrid branch was unreachable —
// remoteQuerier is only constructed when boot.IsRemoteMode() — and
// has been removed.
func buildIdentity(boot *BootstrapConfig, remote types.Querier) identity.Source {
	if boot.IsRemoteMode() {
		return hub.New(remote)
	}
	return local.New(boot.ConfigPath)
}
