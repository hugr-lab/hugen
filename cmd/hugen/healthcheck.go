package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// healthcheckTimeout bounds the probe request. The container HEALTHCHECK's own
// --timeout is the outer bound; this keeps a hung listener from parking the
// probe process indefinitely.
const healthcheckTimeout = 4 * time.Second

// runHealthcheck is the container HEALTHCHECK probe (design-008 M3 / O1): a
// dependency-free GET /healthz against the dedicated API listener. It never
// boots the runtime — the whole point is a cheap liveness check the Docker
// HEALTHCHECK can run on an interval. Exit 0 on HTTP 200, 1 otherwise.
//
// Port comes from HUGEN_API_PORT — the same env `hugen serve` binds its
// dedicated listener to. /healthz is served ONLY in dedicated-listener mode
// (HUGEN_API_PORT > 0; see pkg/adapter/httpapi/adapter.go), so an unset / zero
// port is a hard error: a shared-mux deployment has no health route and must
// not be probed this way.
func runHealthcheck(errOut io.Writer) int {
	port := os.Getenv("HUGEN_API_PORT")
	if port == "" || port == "0" {
		fmt.Fprintln(errOut, "healthcheck: HUGEN_API_PORT must be a nonzero port "+
			"(dedicated-listener mode); /healthz is not served on a shared mux")
		return 1
	}
	return probeHealth(fmt.Sprintf("http://localhost:%s/healthz", port), errOut)
}

// probeHealth issues the GET and maps the outcome to an exit code. Split from
// runHealthcheck so the HTTP behaviour is testable against an exact address
// without the localhost/::1 resolution ambiguity.
func probeHealth(url string, errOut io.Writer) int {
	client := &http.Client{Timeout: healthcheckTimeout}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(errOut, "healthcheck: GET %s: %v\n", url, err)
		return 1
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(errOut, "healthcheck: %s -> %d\n", url, resp.StatusCode)
		return 1
	}
	return 0
}
