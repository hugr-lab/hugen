package httpapi

// The manual skills-refresh path (spec-skills-distribution SK6):
//
//	POST /v1/skills/refresh → run one marketplace reconcile pass now
//
// The out-of-band "re-read config + reconcile installed skills" poke. The
// background reconciler already re-fetches agent_info every cadence tick (SK6
// part a) and the model can call the skill:refresh tool mid-turn; this endpoint
// lets an operator or the hub force a reconcile without either — the manual
// path that did not exist on the hugen side. It reconciles only THIS agent's
// installed hub-tier set against the admin-defined desired-set, so any
// authenticated caller may trigger it (it cannot install anything the desired-
// set/ledger does not already sanction).

import (
	"context"
	"net/http"
	"time"
)

// skillsRefreshTimeout bounds a manual reconcile so a slow/unreachable hub
// cannot hold the request open indefinitely.
const skillsRefreshTimeout = 90 * time.Second

// SkillsRefresher runs one marketplace reconcile pass and returns a compact,
// JSON-marshalable outcome. The cmd layer wires it from the runtime reconciler
// (Core.RefreshSkills); nil leaves the endpoint returning 501.
type SkillsRefresher func(ctx context.Context) (any, error)

// WithSkillsRefresher enables POST /v1/skills/refresh backed by fn. Without it
// the endpoint returns 501 (no marketplace configured).
func WithSkillsRefresher(fn SkillsRefresher) Option {
	return func(a *Adapter) { a.refreshSkills = fn }
}

// handleRefreshSkills serves POST /v1/skills/refresh.
func (a *Adapter) handleRefreshSkills(w http.ResponseWriter, r *http.Request) {
	if a.refreshSkills == nil {
		httpError(w, http.StatusNotImplemented, "no skills marketplace configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), skillsRefreshTimeout)
	defer cancel()
	out, err := a.refreshSkills(ctx)
	if err != nil {
		a.logger.Warn("httpapi: skills refresh failed", "err", err)
		httpError(w, http.StatusBadGateway, "skills refresh failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}
