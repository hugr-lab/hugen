package devui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubStore satisfies auth.TokenStore; Token returns a canned value
// or a canned error.
type stubStore struct {
	tok string
	err error
}

func (s stubStore) Token(context.Context) (string, error) { return s.tok, s.err }

// newRequest builds a request with a loopback-looking RemoteAddr by
// default. Override via opts to force non-loopback.
func newRequest(t *testing.T, url, remoteAddr string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, url, nil)
	r.RemoteAddr = remoteAddr
	return r
}

func TestTokenHandler_LoopbackRequired(t *testing.T) {
	h := TokenHandler(map[string]auth.TokenStore{"hugr": stubStore{tok: "t"}})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequest(t, "/dev/token?name=hugr", "8.8.8.8:12345"))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestTokenHandler_MissingNameSingleAuthUsesIt(t *testing.T) {
	h := TokenHandler(map[string]auth.TokenStore{"hugr": stubStore{tok: "t-1"}})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequest(t, "/dev/token", "127.0.0.1:34567"))
	require.Equal(t, http.StatusOK, w.Code)

	var resp TokenResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "hugr", resp.Name)
	assert.Equal(t, "t-1", resp.AccessToken)
	assert.Equal(t, "Bearer", resp.Type)
}

func TestTokenHandler_MissingNameMultipleAuthsAmbiguous(t *testing.T) {
	h := TokenHandler(map[string]auth.TokenStore{
		"hugr":    stubStore{tok: "t1"},
		"weather": stubStore{tok: "t2"},
	})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequest(t, "/dev/token", "127.0.0.1:34567"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "multiple auth")
}

func TestTokenHandler_UnknownName(t *testing.T) {
	h := TokenHandler(map[string]auth.TokenStore{"hugr": stubStore{tok: "t"}})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequest(t, "/dev/token?name=ghost", "127.0.0.1:34567"))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestTokenHandler_TokenError(t *testing.T) {
	h := TokenHandler(map[string]auth.TokenStore{
		"hugr": stubStore{err: errors.New("refresh failed")},
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequest(t, "/dev/token?name=hugr", "127.0.0.1:34567"))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "refresh failed")
}

func TestTokenHandler_IPv6Loopback(t *testing.T) {
	h := TokenHandler(map[string]auth.TokenStore{"hugr": stubStore{tok: "t"}})
	w := httptest.NewRecorder()
	// RemoteAddr uses bracketed IPv6 form for ports.
	h.ServeHTTP(w, newRequest(t, "/dev/token?name=hugr", "[::1]:34567"))
	assert.Equal(t, http.StatusOK, w.Code)
}

// ---------------------------------------------------------------------
// TriggerAuthHandler
// ---------------------------------------------------------------------

func TestTriggerAuthHandler_RedirectShape(t *testing.T) {
	h := TriggerAuthHandler("http://localhost:10000")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequest(t, "/dev/auth/trigger?name=hugr", "127.0.0.1:1"))
	require.Equal(t, http.StatusFound, w.Code)
	assert.Equal(t, "http://localhost:10000/auth/hugr/login", w.Header().Get("Location"))
}

func TestTriggerAuthHandler_MissingName(t *testing.T) {
	h := TriggerAuthHandler("http://localhost:10000")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequest(t, "/dev/auth/trigger", "127.0.0.1:1"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestTriggerAuthHandler_LoopbackRequired(t *testing.T) {
	h := TriggerAuthHandler("http://localhost:10000")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequest(t, "/dev/auth/trigger?name=hugr", "8.8.8.8:1"))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestTriggerAuthHandler_StripsTrailingSlash(t *testing.T) {
	h := TriggerAuthHandler("http://localhost:10000/")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequest(t, "/dev/auth/trigger?name=weather", "127.0.0.1:1"))
	assert.Equal(t, "http://localhost:10000/auth/weather/login", w.Header().Get("Location"))
}
