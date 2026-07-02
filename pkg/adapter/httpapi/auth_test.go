package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeVerify accepts the token "good" as user u1, rejects everything else.
func fakeVerify(_ context.Context, tok string) (VerifiedUser, error) {
	if tok == "good" {
		return VerifiedUser{UserID: "u1", Name: "Alice", Role: "analyst"}, nil
	}
	return VerifiedUser{}, errors.New("rejected")
}

func whoamiRequest(t *testing.T, a *Adapter, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	if err := a.mount(mux, false); err != nil {
		t.Fatalf("mount: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, whoamiPath, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestWhoami_AllowOpen_DevUser(t *testing.T) {
	// No verifier ⇒ allow-open dev: every request is the local dev user.
	a := New(WithLogger(quietLogger()))
	rec := whoamiRequest(t, a, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	var u VerifiedUser
	if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if u != devUser {
		t.Errorf("whoami = %+v, want devUser %+v", u, devUser)
	}
}

func TestWhoami_Verifier_Valid(t *testing.T) {
	a := New(WithLogger(quietLogger()), WithVerifier(fakeVerify))
	rec := whoamiRequest(t, a, "good")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	var u VerifiedUser
	if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if u.UserID != "u1" || u.Role != "analyst" {
		t.Errorf("whoami = %+v, want u1/analyst", u)
	}
}

func TestWhoami_Verifier_MissingToken(t *testing.T) {
	a := New(WithLogger(quietLogger()), WithVerifier(fakeVerify))
	rec := whoamiRequest(t, a, "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: status %d, want 401", rec.Code)
	}
}

func TestWhoami_Verifier_InvalidToken(t *testing.T) {
	a := New(WithLogger(quietLogger()), WithVerifier(fakeVerify))
	rec := whoamiRequest(t, a, "bad")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("bad token: status %d, want 401", rec.Code)
	}
}

func TestBearerToken(t *testing.T) {
	cases := map[string]string{
		"Bearer abc":  "abc",
		"bearer xyz":  "xyz", // case-insensitive scheme
		"Basic zzz":   "",
		"":            "",
		"Bearer  s ":  "s", // trims
	}
	for header, want := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		if got := bearerToken(r); got != want {
			t.Errorf("bearerToken(%q) = %q, want %q", header, got, want)
		}
	}
}
