// auron-auth performs OIDC authorization-code + PKCE login against Auron and
// writes the resulting tokens to ~/.auron/config.json.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	defaultDiscoveryURL = "https://dev.useauron.ai/api/.well-known/openid-configuration"
	defaultClientID     = "cl_eebdbc2a92f181685a665fd65e663a84"
	defaultScopes       = "openid profile email offline_access"
	redirectURL         = "http://lh.useauron.com:5872/callback"
	listenAddr          = "127.0.0.1:5872" // lh.useauron.com must resolve to 127.0.0.1
)

type discoveryDoc struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

type storedConfig struct {
	Issuer       string    `json:"issuer"`
	ClientID     string    `json:"client_id"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	IDToken      string    `json:"id_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	Scope        string    `json:"scope,omitempty"`
	ObtainedAt   time.Time `json:"obtained_at"`
}

func main() {
	var (
		discoveryURL = flag.String("discovery-url", envOr("AURON_DISCOVERY_URL", defaultDiscoveryURL), "OIDC discovery URL")
		clientID     = flag.String("client-id", envOr("AURON_CLIENT_ID", defaultClientID), "OIDC client_id")
		scopes       = flag.String("scopes", envOr("AURON_SCOPES", defaultScopes), "Space-separated OAuth scopes")
		configPath   = flag.String("config", defaultConfigPath(), "Path to write token config")
		timeout      = flag.Duration("timeout", 5*time.Minute, "Max time to wait for browser callback")
	)
	flag.Parse()

	if err := run(*discoveryURL, *clientID, *scopes, *configPath, *timeout); err != nil {
		fmt.Fprintf(os.Stderr, "auron-auth: %v\n", err)
		os.Exit(1)
	}
}

func run(discoveryURL, clientID, scopes, configPath string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	disc, err := fetchDiscovery(ctx, discoveryURL)
	if err != nil {
		return fmt.Errorf("discovery: %w", err)
	}

	verifier, challenge, err := newPKCE()
	if err != nil {
		return fmt.Errorf("pkce: %w", err)
	}
	state, err := randomString(32)
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}

	authURL := buildAuthURL(disc.AuthorizationEndpoint, clientID, scopes, state, challenge)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv, err := startCallbackServer(state, codeCh, errCh)
	if err != nil {
		return fmt.Errorf("callback server: %w", err)
	}
	defer func() {
		shutdownCtx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = srv.Shutdown(shutdownCtx)
	}()

	fmt.Println("Opening browser to sign in to Auron...")
	fmt.Println("If it doesn't open, paste this URL:")
	fmt.Println(authURL)
	if err := openBrowser(authURL); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not open browser: %v\n", err)
	}

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return fmt.Errorf("timed out waiting for browser callback")
	}

	tok, err := exchangeCode(ctx, disc.TokenEndpoint, clientID, code, verifier)
	if err != nil {
		return fmt.Errorf("token exchange: %w", err)
	}

	now := time.Now().UTC()
	cfg := storedConfig{
		Issuer:       disc.Issuer,
		ClientID:     clientID,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		IDToken:      tok.IDToken,
		TokenType:    tok.TokenType,
		ExpiresAt:    now.Add(time.Duration(tok.ExpiresIn) * time.Second),
		Scope:        tok.Scope,
		ObtainedAt:   now,
	}
	if err := writeConfig(configPath, cfg); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	fmt.Printf("Signed in. Tokens written to %s\n", configPath)
	return nil
}

func fetchDiscovery(ctx context.Context, u string) (*discoveryDoc, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("discovery returned %s: %s", resp.Status, string(body))
	}
	var d discoveryDoc
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, err
	}
	if d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" {
		return nil, errors.New("discovery doc missing authorization_endpoint or token_endpoint")
	}
	return &d, nil
}

func newPKCE() (verifier, challenge string, err error) {
	verifier, err = randomString(64)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b)[:n], nil
}

func buildAuthURL(endpoint, clientID, scopes, state, challenge string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURL)
	q.Set("scope", scopes)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	sep := "?"
	if strings.Contains(endpoint, "?") {
		sep = "&"
	}
	return endpoint + sep + q.Encode()
}

func startCallbackServer(expectedState string, codeCh chan<- string, errCh chan<- error) (*http.Server, error) {
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", listenAddr, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			msg := fmt.Sprintf("OIDC error: %s %s", e, q.Get("error_description"))
			respond(w, msg)
			errCh <- errors.New(msg)
			return
		}
		if q.Get("state") != expectedState {
			respond(w, "State mismatch. You can close this window.")
			errCh <- errors.New("state mismatch")
			return
		}
		code := q.Get("code")
		if code == "" {
			respond(w, "Missing code. You can close this window.")
			errCh <- errors.New("missing code in callback")
			return
		}
		respond(w, "Signed in. You can close this window and return to the terminal.")
		codeCh <- code
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	return srv, nil
}

func respond(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><html><body style="font-family:system-ui;padding:40px"><h2>Auron</h2><p>%s</p></body></html>`, msg)
}

func exchangeCode(ctx context.Context, tokenEndpoint, clientID, code, verifier string) (*tokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", clientID)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURL)
	form.Set("code_verifier", verifier)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("decode token response (%s): %w", resp.Status, err)
	}
	if tr.Error != "" {
		return nil, fmt.Errorf("token endpoint: %s: %s", tr.Error, tr.ErrorDesc)
	}
	if resp.StatusCode != http.StatusOK || tr.AccessToken == "" {
		return nil, fmt.Errorf("token endpoint returned %s: %s", resp.Status, string(body))
	}
	return &tr, nil
}

func writeConfig(path string, cfg storedConfig) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func defaultConfigPath() string {
	if p := os.Getenv("AURON_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".auron/config.json"
	}
	return filepath.Join(home, ".auron", "config.json")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func openBrowser(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	return cmd.Start()
}
