package hugr

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// HugrOIDCConfig is the response from GET {HUGR_URL}/auth/config.
type HugrOIDCConfig struct {
	Issuer   string `json:"issuer"`
	ClientID string `json:"client_id"`
}

// DiscoverOIDCFromHugr fetches OIDC issuer and client_id from the Hugr server.
// Returns nil if Hugr doesn't have OIDC configured (404 or empty response).
func DiscoverOIDCFromHugr(ctx context.Context, hugrURL string) (*HugrOIDCConfig, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hugrURL+"/auth/config", nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s/auth/config: %w", hugrURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // OIDC not configured on Hugr
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hugr /auth/config returned %d", resp.StatusCode)
	}

	var cfg HugrOIDCConfig
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode /auth/config: %w", err)
	}
	if cfg.Issuer == "" || cfg.ClientID == "" {
		return nil, nil // OIDC not configured
	}
	return &cfg, nil
}
