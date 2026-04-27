package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
	src, err := BuildHugrSource(context.Background(), AuthSpec{
		Name:        "hugr",
		Type:        "hugr",
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
	src, err := BuildHugrSource(context.Background(), AuthSpec{
		Name:        "hugr",
		Type:        "hugr",
		DiscoverURL: hugrURL,
		BaseURL:     "http://localhost:10000",
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, "hugr", src.Name())
	_, isOIDC := src.(*OIDCStore)
	assert.True(t, isOIDC, "OIDC fallback yields OIDCStore")
}

func TestBuildHugrSource_MissingName(t *testing.T) {
	_, err := BuildHugrSource(context.Background(), AuthSpec{
		Type: "hugr", AccessToken: "x", TokenURL: "y",
	}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty name")
}

func TestBuildSources_UnknownType(t *testing.T) {
	reg := NewSourceRegistry(nil)
	primary, err := BuildHugrSource(context.Background(), AuthSpec{
		Name: "hugr", Type: "hugr", AccessToken: "s", TokenURL: "http://x/t",
	}, nil)
	require.NoError(t, err)
	require.NoError(t, reg.AddPrimary(primary))

	err = BuildSources(context.Background(), []AuthSpec{
		{Name: "weird", Type: "nope"},
	}, reg, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported type")
}

func TestBuildSources_AliasHugrType(t *testing.T) {
	// Seed registry with a primary Source (any name — alias lookup
	// no longer depends on it being literally "hugr").
	reg := NewSourceRegistry(nil)
	primary, err := BuildHugrSource(context.Background(), AuthSpec{
		Name: "my-hugr", Type: "hugr", AccessToken: "seed", TokenURL: "http://x/token",
	}, nil)
	require.NoError(t, err)
	require.NoError(t, reg.AddPrimary(primary))

	// A provider-auth entry of type=hugr should alias onto the primary.
	require.NoError(t, BuildSources(context.Background(), []AuthSpec{
		{Name: "mcp-inline", Type: "hugr"},
	}, reg, nil))

	got, ok := reg.Source("mcp-inline")
	require.True(t, ok)
	assert.Same(t, primary, got)
}

func TestBuildSources_HugrWithoutPrimary(t *testing.T) {
	reg := NewSourceRegistry(nil)
	err := BuildSources(context.Background(), []AuthSpec{
		{Name: "mcp-inline", Type: "hugr"},
	}, reg, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no primary Source registered")
}

func TestRegistry_AddPrimaryOnce(t *testing.T) {
	reg := NewSourceRegistry(nil)
	a, _ := BuildHugrSource(context.Background(), AuthSpec{Name: "a", Type: "hugr", AccessToken: "x", TokenURL: "y"}, nil)
	b, _ := BuildHugrSource(context.Background(), AuthSpec{Name: "b", Type: "hugr", AccessToken: "x", TokenURL: "y"}, nil)
	require.NoError(t, reg.AddPrimary(a))
	err := reg.AddPrimary(b)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already registered")
	assert.Equal(t, "a", reg.Primary())
}
