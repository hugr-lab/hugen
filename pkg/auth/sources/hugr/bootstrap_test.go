package hugr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hugr-lab/hugen/pkg/auth/sources/oidc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeHugr serves /auth/config with {issuer, client_id} and the OIDC
// discovery document at the returned issuer's /.well-known path —
// enough for NewOIDCStore to succeed without hitting the network.
func fakeHugr(t *testing.T) (hugrURL string) {
	t.Helper()

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Discovery doc under the hugr URL itself — issuer = srv URL keeps
	// things contained.
	mux.HandleFunc("/auth/config", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":    srv.URL,
			"client_id": "agent-client",
		})
	})
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"authorization_endpoint": srv.URL + "/authorize",
			"token_endpoint":         srv.URL + "/token",
		})
	})
	return srv.URL
}

func TestBuildHugrSource_TokenMode(t *testing.T) {
	src, err := BuildHugrSource(context.Background(), Config{
		AccessToken: "seed",
		TokenURL:    "http://localhost:9999/token-exchange",
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, "hugr", src.Name())
	_, isRemote := src.(*RemoteStore)
	assert.True(t, isRemote, "token mode yields RemoteStore")
	assert.False(t, src.OwnsState("hugr.any"), "RemoteStore never owns state")
}

func TestBuildHugrSource_OIDCFallbackDiscovery(t *testing.T) {
	hugrURL := fakeHugr(t)
	src, err := BuildHugrSource(context.Background(), Config{
		DiscoverURL: hugrURL,
		BaseURI:     "http://localhost:10000",
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, "hugr", src.Name())
	_, isOIDC := src.(*oidc.Source)
	assert.True(t, isOIDC, "OIDC fallback yields OIDCStore")
}

func TestBuildHugrSource_MissingName(t *testing.T) {
	_, err := BuildHugrSource(context.Background(), Config{
		AccessToken: "x", TokenURL: "y",
	}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty name")
}
