package skill

// publish.go — the skill:publish runtime tool (spec-skills-distribution SK3).
//
// A runtime-owned tool (the skill:save pattern): the tool CODE tars the
// skill's on-disk bundle and POSTs it to the hub marketplace, so the bundle
// bytes never travel through model context. The model sees only a compact
// status. Auth is the agent JWT (remote) / user token (local) via the "hugr"
// token store — the same authority the tool permission stack uses.
//
// The hub applies the real trust boundary (the §4 publish permission + the
// reserved-name / first-publisher / declared-caps hardening); this side is the
// UX gate — the tool is dispatch-gated on Tier-2 Resolve("hugen:skill",
// "publish") (its PermissionObject) and granted only by a publishing skill.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hugr-lab/hugen/pkg/auth"
	skillpkg "github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// Publisher POSTs an on-disk bundle to the hub marketplace. Nil when no
// marketplace is configured (HUGEN_HUB_URL unset) or no hugr token store is
// wired — skill:publish then answers a clear "not configured" error.
type Publisher struct {
	hubURL string
	client *http.Client
}

// NewPublisher builds a Publisher, or returns nil when publishing is not
// possible (no hub URL / no token store).
func NewPublisher(hubURL string, tokenStore auth.TokenStore) *Publisher {
	hubURL = strings.TrimRight(strings.TrimSpace(hubURL), "/")
	if hubURL == "" || tokenStore == nil {
		return nil
	}
	return &Publisher{
		hubURL: hubURL,
		client: &http.Client{Timeout: 60 * time.Second, Transport: auth.Transport(tokenStore, nil)},
	}
}

// PublishResult mirrors the hub POST /skills/publish success envelope.
type PublishResult struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	ContentHash string `json:"content_hash"`
	Status      string `json:"status"`
}

// publish POSTs the tar.gz bundle and decodes the response. A non-201 status
// is surfaced with the hub's error message so the model can relay why the
// publish was refused (e.g. reserved name, missing capability).
func (p *Publisher) publish(ctx context.Context, tarball []byte) (PublishResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.hubURL+"/skills/publish", bytes.NewReader(tarball))
	if err != nil {
		return PublishResult{}, err
	}
	req.Header.Set("Content-Type", "application/gzip")
	resp, err := p.client.Do(req)
	if err != nil {
		return PublishResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated {
		return PublishResult{}, fmt.Errorf("hub rejected publish (%d): %s", resp.StatusCode, publishErrMsg(body))
	}
	var out PublishResult
	if err := json.Unmarshal(body, &out); err != nil {
		return PublishResult{}, fmt.Errorf("decode publish response: %w", err)
	}
	return out, nil
}

// publishErrMsg pulls the {error:{message}} text out of the hub error envelope,
// falling back to the raw body.
func publishErrMsg(body []byte) string {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err == nil && e.Error.Message != "" {
		if e.Error.Code != "" {
			return e.Error.Code + ": " + e.Error.Message
		}
		return e.Error.Message
	}
	return strings.TrimSpace(string(body))
}

// callPublish resolves the named skill, tars its bundle, and POSTs it to the
// marketplace. User-initiated + approval-gated (see the tool definition).
func (h *SessionSkill) callPublish(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if h.publisher == nil {
		return nil, fmt.Errorf("%w: skill:publish: no marketplace configured (HUGEN_HUB_URL is unset)", tool.ErrSystemUnavailable)
	}
	if h.manager == nil {
		return nil, tool.ErrSystemUnavailable
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("%w: skill:publish: %v", tool.ErrArgValidation, err)
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: skill:publish: name required", tool.ErrArgValidation)
	}
	sk, err := h.manager.Get(ctx, name)
	if err != nil {
		if errors.Is(err, skillpkg.ErrSkillNotFound) {
			return nil, fmt.Errorf("%w: skill:publish: skill %q not found in the store", tool.ErrNotFound, name)
		}
		return nil, fmt.Errorf("skill:publish: %w", err)
	}
	if sk.FS == nil {
		return nil, fmt.Errorf("%w: skill:publish: skill %q has no bundle content to publish", tool.ErrIO, name)
	}
	tarball, err := skillpkg.TarGzBundle(sk.FS)
	if err != nil {
		return nil, fmt.Errorf("%w: skill:publish: tar %q: %v", tool.ErrIO, name, err)
	}
	res, err := h.publisher.publish(ctx, tarball)
	if err != nil {
		return nil, fmt.Errorf("skill:publish: %w", err)
	}
	return json.Marshal(res)
}

// callInstall installs one skill from the marketplace by name (SK4). The
// marketplace client downloads + extracts + ledgers + indexes; the tool
// returns the compact outcome.
func (h *SessionSkill) callInstall(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if h.market == nil {
		return nil, fmt.Errorf("%w: skill:install: no marketplace configured (HUGEN_HUB_URL is unset)", tool.ErrSystemUnavailable)
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("%w: skill:install: %v", tool.ErrArgValidation, err)
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: skill:install: name required", tool.ErrArgValidation)
	}
	out, err := h.market.Install(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("skill:install: %w", err)
	}
	return json.Marshal(out)
}

// callRefresh runs a full marketplace reconcile pass on demand (SK4): pull the
// catalog, upgrade installed skills, re-index. Returns the pass counts.
func (h *SessionSkill) callRefresh(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	if h.market == nil {
		return nil, fmt.Errorf("%w: skill:refresh: no marketplace configured (HUGEN_HUB_URL is unset)", tool.ErrSystemUnavailable)
	}
	out, err := h.market.Refresh(ctx)
	if err != nil {
		return nil, fmt.Errorf("skill:refresh: %w", err)
	}
	return json.Marshal(out)
}
