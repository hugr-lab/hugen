package httpapi

// The skills-panel surface (backs the hub console's per-agent skills view):
//
//	GET  /v1/skills                 → installed skills, grouped by origin/tier
//	GET  /v1/skills/{name}/export   → download the skill bundle as tar.gz
//	POST /v1/skills/install         → upload a tar.gz bundle → install as a
//	                                  local skill (owner/admin; the owner/admin
//	                                  gate is enforced at the hub proxy — hugen
//	                                  only authenticates the forwarded user).
//
// Publish-to-marketplace is NOT here: the console calls the hub's existing
// POST /skills/publish directly (gated on the hugr `hugen:skill.publish` cap).

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing/fstest"

	"github.com/hugr-lab/hugen/pkg/skill"
)

// Install-bundle upload caps — mirror the hub marketplace publish limits so a
// bundle that publishes also installs.
const (
	maxSkillBundleBytes = 32 << 20 // 32 MiB
	maxSkillBundleFiles = 4096
)

// skillManager is the narrow slice of *skill.SkillManager the panel needs:
// list installed skills, fetch one (export), and publish a local one
// (install-from-bundle). Wired from core.Skills; nil ⇒ endpoints return 501.
type skillManager interface {
	List(ctx context.Context) ([]skill.Skill, error)
	Get(ctx context.Context, name string) (skill.Skill, error)
	Publish(ctx context.Context, m skill.Manifest, body fs.FS, opts skill.PublishOptions) error
}

// WithSkillManager enables the skills-panel endpoints (list / export / install).
// Without it they return 501.
func WithSkillManager(m skillManager) Option {
	return func(a *Adapter) { a.skills = m }
}

// skillListItem is one row of the installed-skills panel.
type skillListItem struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Origin       string   `json:"origin"` // system|hub|local|dynamic|inline
	Tiers        []string `json:"tiers"`  // effective tier compatibility
	TaskEligible bool     `json:"task_eligible"`
	Keywords     []string `json:"keywords,omitempty"`
	Exportable   bool     `json:"exportable"` // has a bundle FS (inline skills don't)
	Writable     bool     `json:"writable"`   // local/dynamic → can be overwritten/uninstalled
}

// handleListSkills serves GET /v1/skills.
func (a *Adapter) handleListSkills(w http.ResponseWriter, r *http.Request) {
	if a.skills == nil {
		httpError(w, http.StatusNotImplemented, "skills manager not configured")
		return
	}
	skills, err := a.skills.List(r.Context())
	if err != nil {
		a.logger.Warn("httpapi: list skills", "err", err)
		httpError(w, http.StatusBadGateway, "list skills failed: "+err.Error())
		return
	}
	items := make([]skillListItem, 0, len(skills))
	for _, sk := range skills {
		items = append(items, skillListItem{
			Name:         sk.Manifest.Name,
			Description:  sk.Manifest.Description,
			Origin:       sk.Origin.String(),
			Tiers:        sk.Manifest.EffectiveTierCompatibility(),
			TaskEligible: sk.Manifest.Hugen.Task.Eligible,
			Keywords:     sk.Manifest.Hugen.Mission.Keywords,
			Exportable:   sk.FS != nil,
			Writable:     sk.Origin == skill.OriginLocal || sk.Origin == skill.OriginDynamic,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Origin != items[j].Origin {
			return items[i].Origin < items[j].Origin
		}
		return items[i].Name < items[j].Name
	})
	writeJSON(w, http.StatusOK, map[string]any{"skills": items})
}

// handleExportSkill serves GET /v1/skills/{name}/export — the bundle tar.gz.
func (a *Adapter) handleExportSkill(w http.ResponseWriter, r *http.Request) {
	if a.skills == nil {
		httpError(w, http.StatusNotImplemented, "skills manager not configured")
		return
	}
	name := r.PathValue("name")
	sk, err := a.skills.Get(r.Context(), name)
	if err != nil {
		if errors.Is(err, skill.ErrSkillNotFound) {
			httpError(w, http.StatusNotFound, "skill not found")
			return
		}
		a.logger.Warn("httpapi: get skill for export", "name", name, "err", err)
		httpError(w, http.StatusBadGateway, "get skill failed")
		return
	}
	if sk.FS == nil {
		httpError(w, http.StatusConflict, "skill has no exportable bundle (inline)")
		return
	}
	data, err := skill.TarGzBundle(sk.FS)
	if err != nil {
		a.logger.Warn("httpapi: tar skill bundle", "name", name, "err", err)
		httpError(w, http.StatusInternalServerError, "bundle export failed")
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name+".tar.gz"))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// handleInstallSkill serves POST /v1/skills/install — upload a tar.gz bundle,
// register it as a local skill. ?overwrite=true replaces an existing name.
func (a *Adapter) handleInstallSkill(w http.ResponseWriter, r *http.Request) {
	if a.skills == nil {
		httpError(w, http.StatusNotImplemented, "skills manager not configured")
		return
	}
	body := http.MaxBytesReader(w, r.Body, maxSkillBundleBytes)
	defer func() { _ = r.Body.Close() }()

	tmp, err := os.MkdirTemp("", "skill-install-*")
	if err != nil {
		httpError(w, http.StatusInternalServerError, "temp dir")
		return
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	if err := skill.ExtractTarGz(body, tmp, maxSkillBundleBytes, maxSkillBundleFiles); err != nil {
		httpError(w, http.StatusBadRequest, "invalid bundle: "+err.Error())
		return
	}
	rawMD, err := os.ReadFile(filepath.Join(tmp, "SKILL.md"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "bundle missing SKILL.md")
		return
	}
	manifest, err := skill.Parse(rawMD)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid SKILL.md: "+err.Error())
		return
	}
	// autoload is reserved for system/admin skills compiled into the binary; a
	// hand-installed local skill must load on demand (same rule as skill:save).
	if manifest.Hugen.Autoload || len(manifest.Hugen.AutoloadFor) > 0 {
		httpError(w, http.StatusBadRequest, "autoload is reserved; a locally-installed skill must load on demand")
		return
	}
	bodyFS, err := bundleBodyFromDir(tmp)
	if err != nil {
		a.logger.Warn("httpapi: read bundle body", "err", err)
		httpError(w, http.StatusInternalServerError, "read bundle body")
		return
	}
	overwrite := r.URL.Query().Get("overwrite") == "true"
	if err := a.skills.Publish(r.Context(), manifest, bodyFS, skill.PublishOptions{Overwrite: overwrite}); err != nil {
		switch {
		case errors.Is(err, skill.ErrSkillExists):
			httpError(w, http.StatusConflict, "a skill named "+manifest.Name+" already exists — pass ?overwrite=true to replace")
		case errors.Is(err, skill.ErrUnsupportedBackend):
			httpError(w, http.StatusNotImplemented, "no writable skill backend on this agent")
		case errors.Is(err, skill.ErrAutoloadReserved):
			httpError(w, http.StatusBadRequest, err.Error())
		default:
			a.logger.Warn("httpapi: skill install", "name", manifest.Name, "err", err)
			httpError(w, http.StatusBadGateway, "install failed: "+err.Error())
		}
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"name": manifest.Name, "origin": "local", "status": "installed"})
}

// bundleBodyFromDir builds the body filesystem for a Publish: every regular
// file under dir EXCEPT the root SKILL.md (passed separately as the manifest)
// and any dotfile segment. This preserves references/scripts/assets AND any
// other bundle file (e.g. a root query.graphql) verbatim — a lossless install.
// Mirrors pkg/extension/skill's readBundleBody, which also returns fstest.MapFS.
func bundleBodyFromDir(dir string) (fs.FS, error) {
	body := fstest.MapFS{}
	root := os.DirFS(dir)
	err := fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." || d.IsDir() {
			return nil
		}
		if p == "SKILL.md" || hasDotSegment(p) {
			return nil
		}
		data, err := fs.ReadFile(root, p)
		if err != nil {
			return err
		}
		body[p] = &fstest.MapFile{Data: data}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return body, nil
}

// hasDotSegment reports whether any "/"-separated segment of p starts with a
// dot (a dotfile or dot-directory), matching TarGzBundle's exclusion.
func hasDotSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}
