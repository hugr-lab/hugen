package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// VerifiedUser is the end-user identity a request carries once its forwarded
// token is verified — the SAME shape hugr's auth.me returns. It becomes the
// session OwnerID + ParticipantInfo (H3).
type VerifiedUser struct {
	UserID string `json:"user_id"`
	Name   string `json:"name,omitempty"`
	Role   string `json:"role,omitempty"`
}

// VerifyFunc verifies a raw forwarded user token and returns the identity.
// A non-nil error (or empty UserID) ⇒ 401. The concrete implementation
// (verify against hugr's authority via auth.me) is built in the cmd layer so
// this package stays free of the query-engine / identity deps — D1.
type VerifyFunc func(ctx context.Context, rawToken string) (VerifiedUser, error)

// devUser is the identity injected in allow-open mode (no verifier) — local
// dev only, never a real principal.
var devUser = VerifiedUser{UserID: "local", Name: "local", Role: "local"}

type ctxKey int

const userCtxKey ctxKey = iota

func withUser(ctx context.Context, u VerifiedUser) context.Context {
	return context.WithValue(ctx, userCtxKey, u)
}

// userFromCtx returns the verified user attached by authMiddleware. Handlers
// mounted behind the middleware can rely on ok=true.
func userFromCtx(ctx context.Context) (VerifiedUser, bool) {
	u, ok := ctx.Value(userCtxKey).(VerifiedUser)
	return u, ok
}

// authMiddleware authenticates a request and attaches the verified user to its
// context. With no verifier configured (allow-open dev) it injects devUser.
// Otherwise it requires an `Authorization: Bearer <user-token>` that verifies —
// missing/invalid ⇒ 401. Only protected routes (whoami, sessions) are wrapped;
// the card + health probes stay public.
func (a *Adapter) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.verify == nil {
			next.ServeHTTP(w, r.WithContext(withUser(r.Context(), devUser)))
			return
		}
		tok := bearerToken(r)
		if tok == "" {
			unauthorized(w, "missing bearer token")
			return
		}
		u, err := a.verify(r.Context(), tok)
		if err != nil || u.UserID == "" {
			if err != nil {
				a.logger.Debug("httpapi: token verify failed", "err", err)
			}
			unauthorized(w, "invalid token")
			return
		}
		next.ServeHTTP(w, r.WithContext(withUser(r.Context(), u)))
	})
}

// bearerToken extracts the token from an `Authorization: Bearer <t>` header.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}

func unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	http.Error(w, "unauthorized: "+msg, http.StatusUnauthorized)
}

// whoamiHandler returns the verified identity for the request — the H2 proof of
// the auth pipeline, and a genuinely useful "who does the agent think I am".
func whoamiHandler(w http.ResponseWriter, r *http.Request) {
	u, _ := userFromCtx(r.Context())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(u)
}
