package httpapi

import (
	"io"
	"net/http"
	"os"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// maxArtifactBytes caps an ingest upload.
const maxArtifactBytes = 64 << 20 // 64 MiB

// ArtifactStore is the artifact surface the H6 endpoints need. The cmd layer
// wires it from core.Artifacts (Store.List/Path + Extension.Ingest). rootID is
// the root session id (== the API {id}).
type ArtifactStore interface {
	List(rootID string) ([]protocol.ArtifactRef, error)
	Path(rootID, id string) (string, error)
	Ingest(rootID, srcPath, name string) (protocol.ArtifactRef, error)
}

// handleListArtifacts lists the session's artifacts. H6.
func (a *Adapter) handleListArtifacts(w http.ResponseWriter, r *http.Request) {
	_, id, ok := a.ownedRequest(w, r)
	if !ok {
		return
	}
	if a.artifacts == nil {
		httpError(w, http.StatusNotImplemented, "artifacts disabled")
		return
	}
	refs, err := a.artifacts.List(id)
	if err != nil {
		a.logger.Error("httpapi: list artifacts", "id", id, "err", err)
		httpError(w, http.StatusInternalServerError, "list artifacts failed")
		return
	}
	if refs == nil {
		refs = []protocol.ArtifactRef{}
	}
	writeJSON(w, http.StatusOK, refs)
}

// handleGetArtifact streams one artifact by ref. Auth'd direct download (the
// client already holds the token — no signed URL needed, unlike the A2A FilePart
// leg). Store.Path rejects id traversal. H6.
func (a *Adapter) handleGetArtifact(w http.ResponseWriter, r *http.Request) {
	_, id, ok := a.ownedRequest(w, r)
	if !ok {
		return
	}
	if a.artifacts == nil {
		httpError(w, http.StatusNotImplemented, "artifacts disabled")
		return
	}
	path, err := a.artifacts.Path(id, r.PathValue("aid"))
	if err != nil {
		httpError(w, http.StatusNotFound, "artifact not found")
		return
	}
	http.ServeFile(w, r, path)
}

// handleIngestArtifact ingests an uploaded file into the session's artifact
// scope (raw request body; ?name= for the display name). H6.
func (a *Adapter) handleIngestArtifact(w http.ResponseWriter, r *http.Request) {
	_, id, ok := a.ownedRequest(w, r)
	if !ok {
		return
	}
	if a.artifacts == nil {
		httpError(w, http.StatusNotImplemented, "artifacts disabled")
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "upload"
	}
	tmp, err := os.CreateTemp("", "httpapi-ingest-*")
	if err != nil {
		httpError(w, http.StatusInternalServerError, "ingest failed")
		return
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	_, copyErr := io.Copy(tmp, io.LimitReader(r.Body, maxArtifactBytes))
	_ = tmp.Close()
	if copyErr != nil {
		httpError(w, http.StatusInternalServerError, "ingest read failed")
		return
	}
	ref, err := a.artifacts.Ingest(id, tmpPath, name)
	if err != nil {
		a.logger.Error("httpapi: ingest artifact", "id", id, "err", err)
		httpError(w, http.StatusInternalServerError, "ingest failed")
		return
	}
	writeJSON(w, http.StatusCreated, ref)
}
