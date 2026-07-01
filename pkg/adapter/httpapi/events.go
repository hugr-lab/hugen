package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/hugr-lab/hugen/pkg/session/store"
)

// handleListEvents returns the session's persisted event log — paginated
// history for a UI to load (?from=<seq>&limit=<n>&kinds=a,b). The live
// conversation is the SSE stream (H5); this is the scroll-back / inspection
// surface. Ownership-checked. H6.
func (a *Adapter) handleListEvents(w http.ResponseWriter, r *http.Request) {
	_, id, ok := a.ownedRequest(w, r)
	if !ok {
		return
	}
	opts := store.ListEventsOpts{
		MinSeq: intQuery(r, "from", 0),
		Limit:  intQuery(r, "limit", 0), // 0 ⇒ store default
	}
	if k := strings.TrimSpace(r.URL.Query().Get("kinds")); k != "" {
		opts.Kinds = strings.Split(k, ",")
	}
	rows, err := a.host.ListEvents(r.Context(), id, opts)
	if err != nil {
		a.logger.Error("httpapi: list events", "id", id, "err", err)
		httpError(w, http.StatusInternalServerError, "list events failed")
		return
	}
	if rows == nil {
		rows = []store.EventRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// intQuery reads a non-negative int query param, or def.
func intQuery(r *http.Request, key string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get(key)))
	if err != nil || n < 0 {
		return def
	}
	return n
}
