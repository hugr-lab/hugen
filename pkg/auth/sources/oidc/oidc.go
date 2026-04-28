package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/hugr-lab/hugen/pkg/auth/sources"
)

// inFlightTTL bounds how long a /auth/login state is remembered
// waiting for the matching /auth/callback. Keeps the map from
// growing unbounded when users start a login and never return.
const inFlightTTL = 10 * time.Minute

// inFlightCapacity caps the map size independently of TTL. A single
// operator opening logins in a loop can't balloon memory; LRU
// evicts oldest entries past the cap.
const inFlightCapacity = 64

// Config configures the OIDC browser flow.
type Config struct {
	// Name is the Source identifier. Used for state prefix and
	// registry lookup. Required.
	Name        string
	IssuerURL   string // e.g. http://localhost:8080/realms/hugr
	ClientID    string // Keycloak public client ID
	RedirectURL string // e.g. http://localhost:10000/auth/callback
	// LoginPath is the path that starts the flow (redirect to IdP).
	// Empty defaults to "/auth/login/<Name>".
	LoginPath string
	Logger    *slog.Logger
}

// Source implements Source using Authorization Code + PKCE flow.
// On first Token() call it blocks until the user completes browser
// login. After that it refreshes transparently using the refresh
// token.
type Source struct {
	cfg      Config
	logger   *slog.Logger
	authURL  string // authorization_endpoint
	tokenURL string // token_endpoint

	// Per-in-flight-login state. The dispatcher resolves the Source
	// by state prefix; HandleCallback looks up the verifier by the
	// state's nonce suffix. Expirable LRU auto-evicts stale entries
	// (user started a login + never returned) so the map can't grow
	// unbounded.
	inFlight *expirable.LRU[string, string] // state -> codeVerifier

	tokenMu      sync.Mutex
	accessToken  string
	refreshToken string
	expiresAt    time.Time
	ready        chan struct{} // closed when first login completes
	readyOnce    sync.Once
}

// oidcDiscovery is a subset of OpenID Connect Discovery response.
type oidcDiscovery struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
}

// oidcTokenResponse is the OIDC token endpoint response.
type oidcTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error,omitempty"`
	ErrorDesc    string `json:"error_description,omitempty"`
}

// New creates a store and discovers OIDC endpoints.
// The callback route is mounted by SourceRegistry.Mount on the
// shared /auth/callback path; the login route is mounted per-Source
// at LoginPath() (defaulting to /auth/login/<Name>).
func New(ctx context.Context, cfg Config) (*Source, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("oidc: Name is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	disc, err := discover(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}

	return &Source{
		cfg:      cfg,
		logger:   cfg.Logger,
		authURL:  disc.AuthorizationEndpoint,
		tokenURL: disc.TokenEndpoint,
		inFlight: expirable.NewLRU[string, string](inFlightCapacity, nil, inFlightTTL),
		ready:    make(chan struct{}),
	}, nil
}

// Name implements Source.
func (s *Source) Name() string { return s.cfg.Name }

// Token returns a valid access token. On first call it blocks until
// the user completes browser login. After that it refreshes
// automatically.
func (s *Source) Token(ctx context.Context) (string, error) {
	select {
	case <-s.ready:
	case <-ctx.Done():
		return "", ctx.Err()
	}

	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()

	if time.Now().Before(s.expiresAt) {
		return s.accessToken, nil
	}
	return s.refresh(ctx)
}

// Login implements Source — prints the login URL and opens the
// browser. Safe to call multiple times.
func (s *Source) Login(ctx context.Context) error {
	loginURL := s.loginEndpointURL()
	s.logger.Info("OIDC login required — open in browser",
		"name", s.cfg.Name, "url", loginURL, "client", s.cfg.ClientID)
	fmt.Printf("\n  Login (%s): %s\n\n", s.cfg.Name, loginURL)
	_ = openBrowser(loginURL)
	return nil
}

// OwnsState implements Source using the "<name>.<nonce>" encoding.
func (s *Source) OwnsState(state string) bool {
	return sources.StateOwnedBy(s.cfg.Name, state)
}

// LoginPath returns the path that starts the browser flow.
// Registered on the shared mux by SourceRegistry.Mount.
func (s *Source) LoginPath() string {
	if s.cfg.LoginPath != "" {
		return s.cfg.LoginPath
	}
	return "/auth/login/" + s.cfg.Name
}

// HandleLogin implements LoginHandler — starts the OIDC flow.
func (s *Source) HandleLogin(w http.ResponseWriter, r *http.Request) {
	nonce := generateNonce()
	state := sources.EncodeState(s.cfg.Name, nonce)
	codeVerifier := generateCodeVerifier()

	s.inFlight.Add(state, codeVerifier)

	challenge := computeCodeChallenge(codeVerifier)
	params := url.Values{
		"response_type":         {"code"},
		"client_id":             {s.cfg.ClientID},
		"redirect_uri":          {s.cfg.RedirectURL},
		"scope":                 {"openid"},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	http.Redirect(w, r, s.authURL+"?"+params.Encode(), http.StatusFound)
}

// HandleCallback completes the OIDC flow for this Source. Called
// by the SourceRegistry dispatcher when OwnsState matches.
func (s *Source) HandleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if errParam := q.Get("error"); errParam != "" {
		http.Error(w, fmt.Sprintf("OIDC error: %s — %s", errParam, q.Get("error_description")), http.StatusBadRequest)
		return
	}

	code := q.Get("code")
	state := q.Get("state")
	if code == "" || state == "" {
		http.Error(w, "invalid callback", http.StatusBadRequest)
		return
	}

	codeVerifier, ok := s.inFlight.Peek(state)
	if ok {
		s.inFlight.Remove(state)
	}

	if !ok {
		http.Error(w, "unknown state", http.StatusBadRequest)
		return
	}

	tokens, err := s.exchangeCode(r.Context(), code, codeVerifier)
	if err != nil {
		http.Error(w, "token exchange failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.tokenMu.Lock()
	s.accessToken = tokens.AccessToken
	s.refreshToken = tokens.RefreshToken
	s.expiresAt = time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second)
	s.tokenMu.Unlock()

	s.readyOnce.Do(func() { close(s.ready) })

	s.logger.Info("OIDC login successful", "name", s.cfg.Name)
	w.Header().Set("Content-Type", "text/html")
	_, _ = fmt.Fprint(w, `<html><body><h2>Login successful</h2><p>You can close this tab.</p></body></html>`)
}

// PromptLogin prints the login URL and optionally opens the browser.
// Kept for callers that want the same startup hook as before.
func (s *Source) PromptLogin() {
	_ = s.Login(context.Background())
}

// loginEndpointURL derives the public URL where HandleLogin is
// mounted — RedirectURL host + LoginPath.
func (s *Source) loginEndpointURL() string {
	base := strings.TrimSuffix(s.cfg.RedirectURL, "/auth/callback")
	base = strings.TrimRight(base, "/")
	return base + s.LoginPath()
}

func (s *Source) exchangeCode(ctx context.Context, code, codeVerifier string) (*oidcTokenResponse, error) {
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {s.cfg.ClientID},
		"code":          {code},
		"redirect_uri":  {s.cfg.RedirectURL},
		"code_verifier": {codeVerifier},
	}
	return s.tokenRequest(ctx, data)
}

func (s *Source) refresh(ctx context.Context) (string, error) {
	if s.refreshToken == "" {
		return "", fmt.Errorf("oidc: no refresh token, re-login required")
	}

	data := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {s.cfg.ClientID},
		"refresh_token": {s.refreshToken},
	}

	tokens, err := s.tokenRequest(ctx, data)
	if err != nil {
		return "", err
	}

	s.accessToken = tokens.AccessToken
	if tokens.RefreshToken != "" {
		s.refreshToken = tokens.RefreshToken
	}
	s.expiresAt = time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second)
	return s.accessToken, nil
}

func (s *Source) tokenRequest(ctx context.Context, data url.Values) (*oidcTokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var result oidcTokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("oidc token response: %w", err)
	}
	if result.Error != "" {
		return nil, fmt.Errorf("oidc: %s — %s", result.Error, result.ErrorDesc)
	}
	if result.AccessToken == "" {
		return nil, fmt.Errorf("oidc: empty access_token")
	}
	return &result, nil
}

func discover(ctx context.Context, issuerURL string) (*oidcDiscovery, error) {
	wellKnown := strings.TrimRight(issuerURL, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wellKnown, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discovery returned %d", resp.StatusCode)
	}

	var disc oidcDiscovery
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&disc); err != nil {
		return nil, err
	}
	if disc.AuthorizationEndpoint == "" || disc.TokenEndpoint == "" {
		return nil, fmt.Errorf("missing endpoints in discovery response")
	}
	return &disc, nil
}

func generateCodeVerifier() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func computeCodeChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func generateNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Run()
	case "linux":
		return exec.Command("xdg-open", url).Run()
	default:
		return nil
	}
}
