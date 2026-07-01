package a2a

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Artifact delivery over A2A (A10). A session publishes a file → the artifact
// extension emits an ExtensionFrame{extension:"artifact", op:"artifact_produced",
// data:ArtifactRef} on root's outbox (any tier's publish bubbles to root via the
// F2 marker). The executor maps it to an a2a Artifact carrying a FilePart that
// references the file BY URL — a signed download link served by this adapter.
//
// By-ref (not by-bytes): the bytes never ride the JSON-RPC response; the client
// fetches them from the signed URL. The signature self-authenticates the fetch,
// so the client does NOT need to send the API-key header on the file GET (we
// can't assume a consumer like Copilot propagates connector auth to a FilePart
// URL). The signature is scoped to root|id|exp, so a leaked link grants access
// to exactly one artifact for a bounded window.
const (
	// artifactExtensionName / artifactOpProduced mirror
	// pkg/extension/artifact's providerName + OpProduced (kept as local
	// literals to avoid an adapter→extension import; sanity-checked by test).
	artifactExtensionName = "artifact"
	artifactOpProduced    = "artifact_produced"

	// artifactPathPrefix is where the by-ref download endpoint mounts. It is a
	// subtree of /a2a but NOT behind the API-key header gate — the signed URL
	// is its own capability.
	artifactPathPrefix = "/a2a/artifacts/"

	// artifactURLTTL bounds a signed link's lifetime: generous so a user
	// clicking the file in Teams some time after the turn still fetches it,
	// short enough to limit a leaked URL.
	artifactURLTTL = time.Hour
)

// artifactResolver resolves a (rootID, artifactID) to a readable local path.
// The adapter wires core.Artifacts.Store().Path here.
type artifactResolver func(rootID, id string) (path string, err error)

// randomArtifactSecret returns a fresh signing secret for when no API key is
// configured — artifacts still get signed URLs, they just don't survive a
// restart (acceptable for short-lived links). Fails closed (returns an error)
// rather than installing a known constant secret if crypto/rand is unavailable
// — a forgeable secret is worse than no artifact delivery (L1).
func randomArtifactSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("a2a: artifact signing secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// signArtifact is the HMAC token over root|id|exp.
func signArtifact(secret, root, id string, exp int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%s\n%s\n%d", root, id, exp)
	return hex.EncodeToString(mac.Sum(nil))
}

// signedArtifactURL builds the by-ref download URL a FilePart points at.
func signedArtifactURL(base, secret, root, id string, now time.Time) string {
	exp := now.Add(artifactURLTTL).Unix()
	sig := signArtifact(secret, root, id, exp)
	return fmt.Sprintf("%s%s%s/%s?exp=%d&sig=%s",
		strings.TrimRight(base, "/"), artifactPathPrefix,
		url.PathEscape(root), url.PathEscape(id), exp, sig)
}

// artifactDownloadHandler serves a by-ref artifact after verifying the signed
// URL (root/id from the path, exp+sig from the query). http.ServeFile sets the
// Content-Type from the extension and supports range requests. Phase 8/A10.
func artifactDownloadHandler(secret string, resolve artifactResolver, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, artifactPathPrefix)
		rawRoot, rawID, ok := strings.Cut(rest, "/")
		if !ok || rawRoot == "" || rawID == "" {
			http.Error(w, "bad artifact path", http.StatusBadRequest)
			return
		}
		root, _ := url.PathUnescape(rawRoot)
		id, _ := url.PathUnescape(rawID)
		exp, _ := strconv.ParseInt(r.URL.Query().Get("exp"), 10, 64)
		sig := r.URL.Query().Get("sig")
		if exp == 0 || sig == "" {
			http.Error(w, "missing signature", http.StatusUnauthorized)
			return
		}
		if time.Now().Unix() > exp {
			http.Error(w, "link expired", http.StatusUnauthorized)
			return
		}
		want := signArtifact(secret, root, id, exp)
		if subtle.ConstantTimeCompare([]byte(sig), []byte(want)) != 1 {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		path, err := resolve(root, id)
		if err != nil {
			http.Error(w, "artifact not found", http.StatusNotFound)
			return
		}
		if logger != nil {
			logger.Debug("a2a: serving artifact", "root", root, "id", id)
		}
		http.ServeFile(w, r, path)
	})
}
