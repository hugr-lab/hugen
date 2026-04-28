// Package webui serves the single-page chat UI on a loopback-only
// listener. The static HTML/CSS/JS is embedded via embed.FS; the page
// drives /api/v1/* endpoints exposed by pkg/adapter/http.
package webui

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	stdhttp "net/http"
	"time"

	"github.com/hugr-lab/hugen/pkg/runtime"
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
	logger  *slog.Logger
	handler stdhttp.Handler

	srv *stdhttp.Server
}

// NewAdapter builds the adapter. host defaults to 127.0.0.1 when
// empty — the constitution forbids exposing the dev UI on a public
// interface.
func NewAdapter(host string, port int, logger *slog.Logger) *Adapter {
	if host == "" {
		host = "127.0.0.1"
	}
	if logger == nil {
		logger = slog.Default()
	}
	a := &Adapter{host: host, port: port, logger: logger}
	a.handler = stdhttp.FileServer(stdhttp.FS(StaticFS()))
	return a
}

// Name implements runtime.Adapter.
func (a *Adapter) Name() string { return "webui" }

// Run starts the listener and blocks until ctx is cancelled. Returns
// nil on graceful shutdown, the listener's error otherwise.
//
// host is supplied by the runtime but unused: the webui adapter
// binds its own loopback listener (the static UI surface is
// distinct from the /api/v1 mux).
func (a *Adapter) Run(ctx context.Context, _ runtime.AdapterHost) error {
	a.srv = &stdhttp.Server{
		Addr:    fmt.Sprintf("%s:%d", a.host, a.port),
		Handler: a.handler,
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
