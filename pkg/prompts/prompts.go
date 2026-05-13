// Package prompts renders model-visible prompt templates that
// ship under assets/prompts/. Call sites pass a logical name
// (e.g. "interrupts/stuck_repeated_tool") and a data payload;
// the Renderer reads the bundled template, parses once, caches
// the parsed tree, and executes against data via text/template.
//
// Templates are core agent behaviour wired into the binary —
// not tunable by operators, not materialised to disk. Binary
// upgrades flow through automatically.
//
// Phase 5.1 §α.1; embed-only after 2026-05-13 refresh-fix.
package prompts

import (
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"sync"
	"text/template"
)

// FileExt is the suffix every template file carries. Callers
// pass the name without extension; the loader appends this.
const FileExt = ".tmpl"

// Renderer loads, caches, and executes prompt templates.
//
// Concurrent Render calls are safe. The cache uses sync.Map's
// LoadOrStore so a concurrent first-load race produces at most
// one wasted parse (the loser's parsed template is discarded
// by the runtime).
type Renderer struct {
	embedded fs.FS
	cache    sync.Map
	logger   *slog.Logger
}

// NewRenderer constructs a Renderer over an embedded fs.FS root
// (e.g. fs.Sub(assets.PromptsFS, "prompts")).
//
// Panics if embedded is nil — a Renderer with no source has no
// purpose and a missing wiring should fail loud at boot.
func NewRenderer(embedded fs.FS, logger *slog.Logger) *Renderer {
	if embedded == nil {
		panic("prompts: NewRenderer: embedded fs is nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Renderer{
		embedded: embedded,
		logger:   logger,
	}
}

// Render resolves the named template, executes it against data,
// and returns the rendered string. The name is the logical path
// inside the prompts tree without the .tmpl extension (e.g.
// "interrupts/stuck_repeated_tool").
//
// Errors:
//   - template not found in either override or embedded source
//   - template parse failure
//   - template execution failure (e.g. missing field on data)
//
// Render does not fall back to a literal on error. The caller
// decides whether to log + degrade or to surface the error.
func (r *Renderer) Render(name string, data any) (string, error) {
	t, err := r.lookup(name)
	if err != nil {
		return "", err
	}
	var buf strings.Builder
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("prompts: execute %s: %w", name, err)
	}
	return buf.String(), nil
}

// MustRender is a convenience for compile-time-constant template
// names where a load or render failure is a programming error,
// not a runtime condition. Panics on failure.
func (r *Renderer) MustRender(name string, data any) string {
	out, err := r.Render(name, data)
	if err != nil {
		panic(err)
	}
	return out
}

// lookup returns the parsed template for name, loading and
// caching on first access. Concurrent first-loads parse twice
// at worst; only one parsed value wins the cache slot.
func (r *Renderer) lookup(name string) (*template.Template, error) {
	if v, ok := r.cache.Load(name); ok {
		return v.(*template.Template), nil
	}
	t, err := r.load(name)
	if err != nil {
		return nil, err
	}
	actual, _ := r.cache.LoadOrStore(name, t)
	return actual.(*template.Template), nil
}

// load reads the template body from the embedded FS and parses
// it into a fresh *template.Template named after the logical
// name.
func (r *Renderer) load(name string) (*template.Template, error) {
	relPath := name + FileExt
	body, err := fs.ReadFile(r.embedded, relPath)
	if err != nil {
		return nil, fmt.Errorf("prompts: read embedded %s: %w", relPath, err)
	}
	return parse(name, body)
}

// parse compiles body into a *template.Template named for the
// logical name (so execution errors reference the name, not a
// raw byte offset).
func parse(name string, body []byte) (*template.Template, error) {
	t, err := template.New(name).Parse(string(body))
	if err != nil {
		return nil, fmt.Errorf("prompts: parse %s: %w", name, err)
	}
	return t, nil
}
