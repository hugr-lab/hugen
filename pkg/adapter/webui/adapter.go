// Package webui serves the single-page chat UI on a loopback-only
// listener. The static HTML/CSS/JS is embedded via embed.FS; the page
// drives /api/v1/* endpoints exposed by pkg/adapter/http.
package webui

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	stdhttp "net/http"
	"strings"
	"time"

	"github.com/hugr-lab/hugen/pkg/session/manager"
)

//go:embed static
var staticFS embed.FS

// StaticFS exposes the embedded static directory rooted at static/.
// Tests reach in to assert file presence; production callers
// shouldn't need it directly.
func StaticFS() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// Should be impossible — the directory is embedded at build time.
		panic(fmt.Sprintf("webui: static subtree missing: %v", err))
	}
	return sub
}

// Adapter binds the loopback chat UI on its own listener
// (HUGEN_WEBUI_PORT). The listener is separate from the API listener
// so the static page bypass (no bearer required for /index.html)
// applies only to loopback traffic.
type Adapter struct {
	host    string
	port    int
	apiBase string
	logger  *slog.Logger

	indexTpl *template.Template
	indexBuf []byte // pre-rendered, written on every GET / for cheap reuse
	assets   stdhttp.Handler

	srv *stdhttp.Server
}

// NewAdapter builds the adapter. host defaults to 127.0.0.1 when
// empty — the constitution forbids exposing the dev UI on a public
// interface. apiBase is the API origin the SPA's fetch/EventSource
// calls target (e.g. http://127.0.0.1:10000); it is injected into
// index.html via a <meta name="hugen-api"> tag so the JS does not
// hardcode a port.
func NewAdapter(host string, port int, apiBase string, logger *slog.Logger) *Adapter {
	if host == "" {
		host = "127.0.0.1"
	}
	if logger == nil {
		logger = slog.Default()
	}
	a := &Adapter{
		host:    host,
		port:    port,
		apiBase: strings.TrimRight(apiBase, "/"),
		logger:  logger,
		assets:  stdhttp.FileServer(stdhttp.FS(StaticFS())),
	}
	if err := a.loadIndex(); err != nil {
		// embed.FS files are required at build time; missing one
		// is a packaging defect, not a runtime mistake.
		panic(fmt.Sprintf("webui: load index.html: %v", err))
	}
	return a
}

// loadIndex parses the embedded index.html as a template and
// pre-renders the meta tag with the API base URL.
func (a *Adapter) loadIndex() error {
	raw, err := fs.ReadFile(StaticFS(), "index.html")
	if err != nil {
		return err
	}
	tpl, err := template.New("index.html").Parse(string(raw))
	if err != nil {
		return err
	}
	a.indexTpl = tpl
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, map[string]string{"APIBase": a.apiBase}); err != nil {
		return err
	}
	a.indexBuf = buf.Bytes()
	return nil
}

// serve handles the loopback HTTP request. GET / and /index.html
// return the pre-rendered template; everything else is delegated
// to the embed.FS file server.
func (a *Adapter) serve(w stdhttp.ResponseWriter, r *stdhttp.Request) {
	if r.URL.Path == "/" || r.URL.Path == "/index.html" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.Copy(w, bytes.NewReader(a.indexBuf))
		return
	}
	a.assets.ServeHTTP(w, r)
}

// Name implements manager.Adapter.
func (a *Adapter) Name() string { return "webui" }

// Run starts the listener and blocks until ctx is cancelled. Returns
// nil on graceful shutdown, the listener's error otherwise.
//
// host is supplied by the runtime but unused: the webui adapter
// binds its own loopback listener (the static UI surface is
// distinct from the /api/v1 mux).
func (a *Adapter) Run(ctx context.Context, _ manager.AdapterHost) error {
	a.srv = &stdhttp.Server{
		Addr:    fmt.Sprintf("%s:%d", a.host, a.port),
		Handler: stdhttp.HandlerFunc(a.serve),
	}
	a.logger.Info("webui listening", "addr", a.srv.Addr)

	errCh := make(chan error, 1)
	go func() {
		err := a.srv.ListenAndServe()
		if err != nil && !errors.Is(err, stdhttp.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = a.srv.Shutdown(shutCtx)
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}
