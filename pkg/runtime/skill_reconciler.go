package runtime

// skill_reconciler.go — the agent-side skills reconciler (spec-skills-
// distribution SK2 / SD4).
//
// A background goroutine, started AFTER Build by a long-running adapter (serve
// / tui), that fills the hub-tier install cache (`${STATE}/skills/hub/`) from
// the hub Skills marketplace over HTTP:
//
//   - GET {HubURL}/skills/catalog        → the §4-filtered catalog
//   - GET {HubURL}/skills/{name}/bundle  → the bundle tar.gz
//
// It downloads the desired set (existing installs to upgrade + declared
// desired-set additions), extracts each bundle safely into the hub dir,
// maintains the install ledger (§3), then re-indexes the ledger key set and
// refreshes the store. The first pass runs async so an unreachable hub never
// blocks boot; failures log and retry on the refresh cadence.
//
// Auth is the agent JWT (remote) or the hugr user token (local) via the
// "hugr" token store — the same authority the tool permission stack uses.
// Local mode is best-effort: no ready token → skip + retry (SD7).

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/skill"
)

const (
	// defaultSkillRefresh is the reconcile cadence (SD4: boot + every 30m +
	// on-demand trigger). YAML `skills.refresh` wiring is a follow-up.
	defaultSkillRefresh = 30 * time.Minute
	// perPassTimeout bounds one reconcile pass so a hung hub / stuck token
	// exchange never wedges the goroutine (SD7 short-ctx probe).
	perPassTimeout = 2 * time.Minute
	// maxBundleBytes / maxBundleFiles cap a single extracted bundle — a
	// defense against a decompression bomb or a runaway upload (SK2
	// acceptance: safe extraction).
	maxBundleBytes = 32 << 20 // 32 MiB
	maxBundleFiles = 4096
)

// Compile-time assertion: the reconciler is the marketplace client the skill
// extension's on-demand tools drive.
var _ skill.Marketplace = (*skillReconciler)(nil)

// skillReconciler holds the reconcile loop's dependencies.
type skillReconciler struct {
	hubURL  string
	hubDir  string
	client  *http.Client
	store   *skill.Store
	skills  config.SkillsView
	refresh time.Duration
	log     loggerIface

	trigger chan struct{}
}

// loggerIface is the slice of *slog.Logger the reconciler uses (kept as an
// interface only so tests can substitute a recorder — production passes the
// real logger).
type loggerIface interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Debug(msg string, args ...any)
}

// newSkillReconciler builds the marketplace reconciler/client when a
// marketplace is configured (Cfg.Hugr.HubURL set), a dynamic skill store is
// wired, and a hugr token store exists. Returns nil otherwise — the embedded
// seed remains the baseline and skill:install/refresh answer "not configured".
// Built during Build (phaseExtensions) so it can back the skill extension's
// on-demand tools; the cadence loop is started separately by
// StartSkillReconciler on the serve/tui path.
func newSkillReconciler(c *Core) *skillReconciler {
	if c.Cfg.Hugr.HubURL == "" {
		return nil
	}
	store, ok := c.SkillStore.(*skill.Store)
	if !ok || !store.HasDynamic() {
		return nil
	}
	if c.Auth == nil {
		return nil
	}
	tokenStore, ok := c.Auth.TokenStore("hugr")
	if !ok {
		return nil
	}
	return &skillReconciler{
		hubURL:  strings.TrimRight(c.Cfg.Hugr.HubURL, "/"),
		hubDir:  filepath.Join(c.Cfg.StateDir, "skills/hub"),
		client:  &http.Client{Timeout: 60 * time.Second, Transport: auth.Transport(tokenStore, nil)},
		store:   store,
		skills:  c.Config.Skills(),
		refresh: defaultSkillRefresh,
		log:     c.Logger,
		trigger: make(chan struct{}, 1),
	}
}

// StartSkillReconciler starts the background cadence loop over the reconciler
// built at Build time (c.skillRec). A no-op when no marketplace is configured.
// Registers a cleanup that stops the goroutine on Shutdown. Safe to call once
// per long-running adapter path (serve / tui).
func (c *Core) StartSkillReconciler(ctx context.Context) {
	r := c.skillRec
	if r == nil {
		c.Logger.Debug("skill reconciler: disabled (no marketplace configured)")
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	c.addCleanup(cancel)
	go r.run(runCtx)
	c.Logger.Info("skill reconciler: started", "hub", r.hubURL, "refresh", r.refresh)
}

// run is the reconcile loop: an immediate async first pass, then on the
// refresh ticker or an on-demand trigger, until ctx is cancelled.
func (r *skillReconciler) run(ctx context.Context) {
	r.passWithTimeout(ctx)
	ticker := time.NewTicker(r.refresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.passWithTimeout(ctx)
		case <-r.trigger:
			r.passWithTimeout(ctx)
		}
	}
}

// Trigger requests an out-of-cadence reconcile (coalesced: a pending trigger
// is not doubled). Non-blocking.
func (r *skillReconciler) Trigger() {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

func (r *skillReconciler) passWithTimeout(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, perPassTimeout)
	defer cancel()
	if _, err := r.reconcileOnce(ctx); err != nil {
		r.log.Warn("skill reconciler: pass had errors", "err", err)
	}
}

// Refresh runs one reconcile pass on demand (the skill:refresh tool). It also
// nudges the cadence loop so the background schedule stays aligned. Implements
// [skill.Marketplace].
func (r *skillReconciler) Refresh(ctx context.Context) (skill.RefreshOutcome, error) {
	r.Trigger()
	return r.reconcileOnce(ctx)
}

// reconcileOnce runs one full pass: fetch catalog → download the desired set
// → index the ledger keys → save the ledger. Best-effort per skill. Returns
// the per-pass counts.
func (r *skillReconciler) reconcileOnce(ctx context.Context) (skill.RefreshOutcome, error) {
	catalog, err := r.fetchCatalog(ctx)
	if err != nil {
		return skill.RefreshOutcome{}, fmt.Errorf("fetch catalog: %w", err)
	}
	ledger, err := skill.LoadLedger(r.hubDir)
	if err != nil {
		return skill.RefreshOutcome{}, fmt.Errorf("load ledger: %w", err)
	}

	declared := r.skills.InstallSetDeclared()
	wantInstall := map[string]struct{}{}
	if declared {
		for _, n := range r.skills.InstallSet() {
			wantInstall[n] = struct{}{}
		}
	}

	var out skill.RefreshOutcome
	for _, entry := range catalog {
		origin, want := decideFetch(entry.Name, ledger, declared, wantInstall)
		if !want {
			continue
		}
		// Skip when the ledger already records this exact content on disk.
		if led, ok := ledger.Get(entry.Name); ok && led.Hash == entry.ContentHash {
			if _, statErr := os.Stat(filepath.Join(r.hubDir, entry.Name)); statErr == nil {
				continue
			}
		}
		newHash, err := r.installBundle(ctx, entry.Name, entry.ContentHash)
		if err != nil {
			r.log.Warn("skill reconciler: install failed", "skill", entry.Name, "err", err)
			out.Failed++
			continue
		}
		if _, existed := ledger.Get(entry.Name); existed {
			out.Upgraded++
		} else {
			out.Downloaded++
		}
		ledger.Set(entry.Name, skill.LedgerEntry{Hash: newHash, Origin: origin, InstalledAt: nowStamp()})
	}

	if err := ledger.Save(); err != nil {
		return out, fmt.Errorf("save ledger: %w", err)
	}

	// Re-index the authoritative installed set (ledger keys) and refresh so
	// self-installed and freshly downloaded bundles are always discoverable.
	n, ierr := r.store.IndexHubBundles(ctx, r.hubDir, ledger.Names())
	if ierr != nil {
		r.log.Warn("skill reconciler: index re-sync had errors", "indexed", n, "err", ierr)
	}
	r.log.Info("skill reconciler: pass complete",
		"downloaded", out.Downloaded, "upgraded", out.Upgraded, "failed", out.Failed, "indexed", n)
	return out, nil
}

// Install pulls one named skill from the catalog into the installed tier on
// demand (the skill:install tool). A new name lands as origin=self; an
// existing install keeps its origin (a seed stays a seed) and is upgraded to
// the catalog's content. Implements [skill.Marketplace].
func (r *skillReconciler) Install(ctx context.Context, name string) (skill.InstallOutcome, error) {
	catalog, err := r.fetchCatalog(ctx)
	if err != nil {
		return skill.InstallOutcome{}, fmt.Errorf("fetch catalog: %w", err)
	}
	var entry *catalogEntry
	for i := range catalog {
		if catalog[i].Name == name {
			entry = &catalog[i]
			break
		}
	}
	if entry == nil {
		return skill.InstallOutcome{}, fmt.Errorf("skill %q is not in the marketplace catalog (or you lack the capability to see it)", name)
	}

	ledger, err := skill.LoadLedger(r.hubDir)
	if err != nil {
		return skill.InstallOutcome{}, fmt.Errorf("load ledger: %w", err)
	}
	origin := skill.InstallSelf
	if existing, ok := ledger.Get(name); ok {
		origin = existing.Origin // upgrade in place, preserve origin
		if existing.Hash == entry.ContentHash {
			if _, statErr := os.Stat(filepath.Join(r.hubDir, name)); statErr == nil {
				return skill.InstallOutcome{Name: name, Version: entry.Version, ContentHash: entry.ContentHash, AlreadyCurrent: true}, nil
			}
		}
	}

	newHash, err := r.installBundle(ctx, name, entry.ContentHash)
	if err != nil {
		return skill.InstallOutcome{}, err
	}
	ledger.Set(name, skill.LedgerEntry{Hash: newHash, Origin: origin, InstalledAt: nowStamp()})
	if err := ledger.Save(); err != nil {
		return skill.InstallOutcome{}, fmt.Errorf("save ledger: %w", err)
	}
	if _, ierr := r.store.IndexHubBundles(ctx, r.hubDir, ledger.Names()); ierr != nil {
		return skill.InstallOutcome{}, fmt.Errorf("index: %w", ierr)
	}
	return skill.InstallOutcome{Name: name, Version: entry.Version, ContentHash: newHash}, nil
}

// decideFetch decides whether a catalog entry should be fetched and, if so,
// under which install origin. An existing ledger entry is upgraded in place
// with its origin PRESERVED (a seed stays a seed on catalog drift, §3); a new
// name is fetched only when the operator's declared install-set names it
// (origin=desired). A catalog-only name with no ledger entry and no declared
// request is left alone — the reconciler is additive to the seed baseline,
// not an "install everything anyone published" pull.
func decideFetch(name string, ledger *skill.Ledger, declared bool, wantInstall map[string]struct{}) (skill.InstallOrigin, bool) {
	if entry, ok := ledger.Get(name); ok {
		return entry.Origin, true
	}
	if declared {
		if _, ok := wantInstall[name]; ok {
			return skill.InstallDesired, true
		}
	}
	return "", false
}

// installBundle downloads {name}'s bundle, extracts it safely into a temp dir
// under the hub root, verifies the extracted content hash against the catalog
// hash, then atomically swaps it into place. Returns the verified canonical
// hash. A hash mismatch is a hard error (the bundle is rejected, not indexed).
func (r *skillReconciler) installBundle(ctx context.Context, name, wantHash string) (string, error) {
	body, err := r.downloadBundle(ctx, name)
	if err != nil {
		return "", err
	}
	defer func() { _ = body.Close() }()

	tmp := filepath.Join(r.hubDir, ".tmp-"+name)
	if err := os.RemoveAll(tmp); err != nil {
		return "", fmt.Errorf("clean tmp: %w", err)
	}
	if err := safeExtractTarGz(body, tmp, maxBundleBytes, maxBundleFiles); err != nil {
		_ = os.RemoveAll(tmp)
		return "", fmt.Errorf("extract: %w", err)
	}

	gotHash, err := skill.BundleHash(os.DirFS(tmp))
	if err != nil {
		_ = os.RemoveAll(tmp)
		return "", fmt.Errorf("hash extracted: %w", err)
	}
	if wantHash != "" && gotHash != wantHash {
		_ = os.RemoveAll(tmp)
		return "", fmt.Errorf("content hash mismatch: catalog %s, extracted %s", wantHash, gotHash)
	}
	// Debug sentinel (dotfile → excluded from the hash) for parity with the
	// seed; written before the swap so the live dir is never half-written.
	if err := os.WriteFile(filepath.Join(tmp, ".hugen-checksum"), []byte(gotHash+"\n"), 0o644); err != nil {
		_ = os.RemoveAll(tmp)
		return "", fmt.Errorf("write sentinel: %w", err)
	}

	dst := filepath.Join(r.hubDir, name)
	if err := os.RemoveAll(dst); err != nil {
		_ = os.RemoveAll(tmp)
		return "", fmt.Errorf("clean dst: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.RemoveAll(tmp)
		return "", fmt.Errorf("commit bundle: %w", err)
	}
	return gotHash, nil
}

// catalogEntry mirrors the hub's GET /skills/catalog listing shape (only the
// fields the reconciler needs).
type catalogEntry struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	ContentHash string `json:"content_hash"`
}

func (r *skillReconciler) fetchCatalog(ctx context.Context) ([]catalogEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.hubURL+"/skills/catalog", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalog status %d", resp.StatusCode)
	}
	var out struct {
		Skills []catalogEntry `json:"skills"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBundleBytes)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode catalog: %w", err)
	}
	return out.Skills, nil
}

// downloadBundle GETs {name}'s bundle tar.gz. Caller closes the body.
func (r *skillReconciler) downloadBundle(ctx context.Context, name string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.hubURL+"/skills/"+name+"/bundle", nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("bundle status %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// safeExtractTarGz extracts a gzip-compressed tar into dst, rejecting any
// entry that would escape dst (absolute path, "..", or a symlink/hardlink),
// enforcing a cumulative byte cap and a file-count cap. Only regular files
// and directories are materialised — link/device/fifo entries are refused so
// a malicious bundle cannot plant a symlink or write outside the tree.
func safeExtractTarGz(r io.Reader, dst string, maxBytes int64, maxFiles int) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)

	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("mkdir dst: %w", err)
	}
	root, err := filepath.Abs(dst)
	if err != nil {
		return err
	}

	var total int64
	var files int
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir {
			return fmt.Errorf("unsafe entry type %d in %q", hdr.Typeflag, hdr.Name)
		}
		clean := filepath.Clean(hdr.Name)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe path %q", hdr.Name)
		}
		target := filepath.Join(root, clean)
		// Defense in depth: the joined target must stay within root.
		if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
			return fmt.Errorf("path escapes bundle root: %q", hdr.Name)
		}

		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("mkdir %q: %w", target, err)
			}
			continue
		}
		files++
		if files > maxFiles {
			return fmt.Errorf("too many files (> %d)", maxFiles)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("mkdir parent %q: %w", target, err)
		}
		remaining := maxBytes - total
		if remaining <= 0 {
			return fmt.Errorf("bundle exceeds size cap (%d bytes)", maxBytes)
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return fmt.Errorf("create %q: %w", target, err)
		}
		n, err := io.Copy(f, io.LimitReader(tr, remaining+1))
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("write %q: %w", target, err)
		}
		total += n
		if total > maxBytes {
			return fmt.Errorf("bundle exceeds size cap (%d bytes)", maxBytes)
		}
	}
	return nil
}
