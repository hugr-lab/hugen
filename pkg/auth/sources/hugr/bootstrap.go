package hugr

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/hugr-lab/hugen/pkg/auth/sources"
	"github.com/hugr-lab/hugen/pkg/auth/sources/oidc"
)

type Config struct {
	DiscoverURL string `json:"discover_url,omitempty"`
	Issuer      string `json:"issuer,omitempty"`
	ClientID    string `json:"client_id,omitempty"`

	AccessToken string `json:"access_token,omitempty"`
	TokenURL    string `json:"token_url,omitempty"`

	// TokenCacheFile enables the persisted-token fast path in token mode
	// (spec-hub-side §1.5): the current agent JWT is cached here (0600)
	// after every exchange and read back on boot, so a restarted container
	// whose one-shot bootstrap secret is already consumed still refreshes.
	TokenCacheFile string `json:"token_cache_file,omitempty"`

	RedirectURI string `json:"redirect_uri,omitempty"` // for OIDC mode; if empty, set to {BaseURL}/auth/callback
	BaseURI     string `json:"base_url,omitempty"`     // for OIDC redirect URL derivation
}

// BuildHugrSource builds the single Source needed for the hugr
// connection (Phase A of the startup sequence). Chooses between
// RemoteStore (when AccessToken + TokenURL are set) and OIDCStore
// with discovery through {DiscoverURL}/auth/config.
//
// The returned Source is NOT yet registered in any SourceRegistry
// — callers pass it to reg.Add and reg.Mount.
func BuildHugrSource(ctx context.Context, config Config, logger *slog.Logger) (sources.Source, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if config.AccessToken != "" && config.TokenURL != "" {
		// Bootstrap semantics: HUGR_ACCESS_TOKEN may be a one-shot secret
		// rather than a usable bearer, so the first Token() call exchanges
		// immediately instead of serving the env value for a grace window.
		// A cached token from a previous run (WithTokenCache) takes
		// precedence over the env credential — see spec-hub-side §1.5.
		store := NewRemoteStoreBootstrap("hugr", config.AccessToken, config.TokenURL)
		if config.TokenCacheFile != "" {
			store = store.WithTokenCache(config.TokenCacheFile)
		}
		logger.Info("auth source built",
			"name", "hugr", "type", "hugr", "mode", "token",
			"token_url", config.TokenURL, "token_cache", config.TokenCacheFile != "")
		return store, nil
	}

	issuer := config.Issuer
	clientID := config.ClientID
	if (issuer == "" || clientID == "") && config.DiscoverURL != "" {
		disc, err := DiscoverOIDCFromHugr(ctx, config.DiscoverURL)
		if err != nil {
			return nil, fmt.Errorf("auth %q: discover from %s: %w", "hugr", config.DiscoverURL, err)
		}
		if disc == nil {
			return nil, fmt.Errorf("auth %q: discover from %s returned empty config", "hugr", config.DiscoverURL)
		}
		if issuer == "" {
			issuer = disc.Issuer
		}
		if clientID == "" {
			clientID = disc.ClientID
		}
	}
	if issuer == "" || clientID == "" {
		return nil, fmt.Errorf("auth %q: no token_url/access_token and discovery did not yield issuer+client_id", "hugr")
	}
	if config.RedirectURI == "" && config.BaseURI != "" {
		config.RedirectURI = fmt.Sprintf("%s/auth/callback", config.BaseURI)
	}
	cfg := oidc.Config{
		Name:        "hugr",
		IssuerURL:   issuer,
		ClientID:    clientID,
		RedirectURL: config.RedirectURI,
		Logger:      logger.With("auth", "hugr"),
	}
	store, err := oidc.New(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("auth hugr: %w", err)
	}
	logger.Info("auth source built",
		"name", "hugr", "type", "hugr", "mode", "oidc", "issuer", issuer)
	return store, nil
}
