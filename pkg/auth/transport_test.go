package auth

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHeaderTransport_InjectsHeader(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := &http.Client{Transport: HeaderTransport("X-API-Key", "secret-42", nil)}
	resp, err := c.Get(srv.URL + "/ping")
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	assert.Equal(t, "secret-42", got.Get("X-API-Key"))
}

func TestHeaderTransport_NilBaseDefaults(t *testing.T) {
	rt := HeaderTransport("X-Foo", "bar", nil)
	assert.NotNil(t, rt)
}
