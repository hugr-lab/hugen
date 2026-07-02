package httpapi

import (
	_ "embed"
	"net/http"
)

// uiHTML is the minimal dev client (H9) served at /ui in allow-open mode — the
// test-ladder seed + multi-interface proof, NOT the production UI.
//
//go:embed ui.html
var uiHTML []byte

const uiPath = "/ui"

func serveUI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(uiHTML)
}
