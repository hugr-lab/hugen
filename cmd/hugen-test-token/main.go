// Package main is the hugen-test-token utility — a one-shot OIDC
// browser-flow runner that captures fresh access/refresh tokens
// against a Hugr instance and writes them into the harness's
// tests/scenarios/.test.env so subsequent `make scenario` runs can
// hit hugr-protected endpoints without an interactive login.
//
// Why a separate binary instead of a flag on `hugen`: production
// hugen runs the OIDC flow on first request and refreshes
// transparently for the lifetime of the process. The harness needs
// the *captured* tokens on disk so the same value survives across
// test runs (and across reboots, for as long as the refresh token
// chain holds). This binary mounts the same Source production uses,
// drives the flow once, and dumps the result.
//
// Usage:
//
//	make hugr-token            # uses tests/scenarios/.test.env
//	go run ./cmd/hugen-test-token --env-file=tests/scenarios/.test.env
//	go run ./cmd/hugen-test-token --discover-url=http://host.docker.internal:15004
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/hugr-lab/hugen/pkg/auth/sources/hugr"
	"github.com/hugr-lab/hugen/pkg/auth/sources/oidc"
)

const (
	exitOK    = 0
	exitUsage = 64
	exitErr   = 1
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, errOut *os.File) int {
	fs := flag.NewFlagSet("hugen-test-token", flag.ContinueOnError)
	fs.SetOutput(errOut)
	envFile := fs.String("env-file", "tests/scenarios/.test.env",
		"path to the .test.env file the harness reads (HUGR_* keys are written here)")
	discoverURL := fs.String("discover-url", "",
		"Hugr base URL whose /auth/config returns issuer + client_id; "+
			"defaults to HUGR_URL or HUGR_DISCOVER_URL from --env-file")
	autoOpen := fs.Bool("auto-open-browser", true,
		"open the OS default browser to the login URL")
	timeout := fs.Duration("timeout", 5*time.Minute,
		"how long to wait for the user to complete the OIDC flow")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}

	logger := slog.New(slog.NewTextHandler(errOut, &slog.HandlerOptions{Level: slog.LevelInfo}))

	envPath, err := filepath.Abs(*envFile)
	if err != nil {
		fmt.Fprintf(errOut, "resolve env-file: %v\n", err)
		return exitErr
	}

	envKV, _ := readDotEnv(envPath)

	if *discoverURL == "" {
		// Order matches the harness contract: explicit override → .test.env →
		// fall back to the user's process env so a developer can `export
		// HUGR_URL=...` and run the binary without writing to disk first.
		for _, key := range []string{"HUGR_DISCOVER_URL", "HUGR_URL"} {
			if v := envKV[key]; v != "" {
				*discoverURL = v
				break
			}
			if v := os.Getenv(key); v != "" {
				*discoverURL = v
				break
			}
		}
	}
	if *discoverURL == "" {
		fmt.Fprintln(errOut,
			"no discover URL: set --discover-url, HUGR_DISCOVER_URL, or HUGR_URL in --env-file")
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Probe a free local port for the callback redirect. Hugen-proper
	// uses HUGEN_PORT (10000); we pick a fresh one so the binary
	// works alongside a running hugen without colliding.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(errOut, "bind callback listener: %v\n", err)
		return exitErr
	}
	port := listener.Addr().(*net.TCPAddr).Port
	baseURL := fmt.Sprintf("http://localhost:%d", port)

	logger.Info("discovering OIDC config", "url", *discoverURL+"/auth/config")
	source, err := hugr.BuildHugrSource(ctx, hugr.Config{
		DiscoverURL: *discoverURL,
		BaseURI:     baseURL,
	}, logger)
	if err != nil {
		_ = listener.Close()
		fmt.Fprintf(errOut, "build hugr source: %v\n", err)
		return exitErr
	}
	store, ok := source.(*oidc.Source)
	if !ok {
		_ = listener.Close()
		fmt.Fprintln(errOut,
			"hugr discovery returned a static-token source — nothing to capture; "+
				"clear HUGR_ACCESS_TOKEN/HUGR_TOKEN_URL from env to force OIDC mode")
		return exitErr
	}

	mux := http.NewServeMux()
	mux.HandleFunc(store.LoginPath(), store.HandleLogin)
	mux.HandleFunc("/auth/callback", store.HandleCallback)

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("callback server", "err", err)
		}
	}()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	loginURL := baseURL + store.LoginPath()
	fmt.Fprintf(errOut, "\nLogin URL: %s\n\n", loginURL)
	if *autoOpen {
		if err := openBrowser(loginURL); err != nil {
			logger.Warn("auto-open-browser failed; copy the URL above into your browser", "err", err)
		}
	}

	loginCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	// Token() blocks on the source's ready channel — closes only after
	// HandleCallback finishes the code exchange. Once it returns, the
	// raw access/refresh/expiresAt fields are populated and Tokens()
	// returns them without going to the network.
	if _, err := store.Token(loginCtx); err != nil {
		fmt.Fprintf(errOut, "wait for browser login: %v\n", err)
		return exitErr
	}

	access, refresh, expiresAt, err := store.Tokens()
	if err != nil {
		fmt.Fprintf(errOut, "Tokens(): %v\n", err)
		return exitErr
	}

	updates := map[string]string{
		"HUGR_DISCOVER_URL":      *discoverURL,
		"HUGR_URL":               *discoverURL,
		"HUGR_ACCESS_TOKEN":      access,
		"HUGR_REFRESH_TOKEN":     refresh,
		"HUGR_TOKEN_EXPIRES_AT":  expiresAt.UTC().Format(time.RFC3339),
	}
	if err := writeDotEnv(envPath, updates); err != nil {
		fmt.Fprintf(errOut, "write %s: %v\n", envPath, err)
		return exitErr
	}

	ttl := time.Until(expiresAt).Round(time.Second)
	fmt.Fprintf(errOut, "✓ Token captured (expires in %s)\n", ttl)
	fmt.Fprintf(errOut, "✓ %s updated with HUGR_ACCESS_TOKEN, HUGR_REFRESH_TOKEN, HUGR_TOKEN_EXPIRES_AT\n",
		envPath)
	return exitOK
}

// readDotEnv parses a .env-style file into key/value pairs. Lines
// that don't match KEY=VALUE are returned via the second slice in
// arrival order so writeDotEnv can preserve comments + blank lines.
// Missing files return an empty map without error.
func readDotEnv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()

	kv := make(map[string]string)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"`)
		kv[key] = val
	}
	return kv, sc.Err()
}

// writeDotEnv updates .env-style file at path with the given keys.
// Existing keys are replaced in place; new keys append at the end.
// Comments + blank lines + unrelated keys are preserved verbatim.
func writeDotEnv(path string, updates map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	var existingLines []string
	if data, err := os.ReadFile(path); err == nil {
		existingLines = strings.Split(string(data), "\n")
		// strings.Split on a trailing newline produces an empty
		// element; drop it so we don't write a double blank.
		if n := len(existingLines); n > 0 && existingLines[n-1] == "" {
			existingLines = existingLines[:n-1]
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	seen := make(map[string]bool, len(updates))
	out := make([]string, 0, len(existingLines)+len(updates))
	for _, line := range existingLines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			out = append(out, line)
			continue
		}
		eq := strings.IndexByte(trimmed, '=')
		if eq <= 0 {
			out = append(out, line)
			continue
		}
		key := strings.TrimSpace(trimmed[:eq])
		if newVal, ok := updates[key]; ok {
			out = append(out, key+"="+newVal)
			seen[key] = true
			continue
		}
		out = append(out, line)
	}
	for _, key := range orderedKeys(updates) {
		if seen[key] {
			continue
		}
		out = append(out, key+"="+updates[key])
	}

	tmp := path + ".tmp"
	body := strings.Join(out, "\n") + "\n"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func orderedKeys(updates map[string]string) []string {
	// Stable order keeps the diff readable when the binary appends
	// fresh keys to a file that didn't have them. Sort by name —
	// alphabetical groups HUGR_* together which is what the human
	// reader expects.
	keys := make([]string, 0, len(updates))
	for k := range updates {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}

func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Run()
	case "linux":
		return exec.Command("xdg-open", url).Run()
	default:
		return fmt.Errorf("auto-open not supported on %s; open the URL manually", runtime.GOOS)
	}
}
