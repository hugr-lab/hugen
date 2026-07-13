package skill

import "context"

// marketplace.go — the domain-level contract for on-demand marketplace
// operations (spec-skills-distribution SK4). The runtime's reconciler
// implements it; the skill extension consumes it through this interface so the
// model-facing skill:install / skill:refresh tools stay decoupled from the
// runtime's HTTP + cadence machinery. Both sides depend only on pkg/skill.

// InstallOutcome is the compact result of a single skill:install.
type InstallOutcome struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	ContentHash string `json:"content_hash"`
	// AlreadyCurrent is true when the named skill was already installed at the
	// catalog's content hash (the install was a no-op).
	AlreadyCurrent bool `json:"already_current"`
}

// RefreshOutcome is the compact result of a skill:refresh (one reconcile pass).
type RefreshOutcome struct {
	Downloaded int `json:"downloaded"`
	Upgraded   int `json:"upgraded"`
	// Removed counts desired-origin installs retired because the operator
	// dropped them from skills.install (SK6 removal-on-drop).
	Removed int `json:"removed"`
	Failed  int `json:"failed"`
}

// Marketplace is the on-demand side-band the skill extension drives. Install
// pulls one named skill from the hub catalog into the installed tier (origin
// self, or the existing origin on an upgrade); Refresh runs a full reconcile
// pass now. Implementations own the HTTP fetch, safe extraction, ledger write,
// and index re-sync — the tool sees only the compact outcome.
type Marketplace interface {
	Install(ctx context.Context, name string) (InstallOutcome, error)
	Refresh(ctx context.Context) (RefreshOutcome, error)
}
