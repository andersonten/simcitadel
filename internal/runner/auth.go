package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/andersonten/simcitadel/internal/config"
)

// refreshMargin is the lead time of SDD section 9, which refreshes the token
// 60 s before it expires.
const refreshMargin = 60 * time.Second

// TokenSource holds the bearer token of one run and renews it before expiry.
// SDD section 9 caches the token in memory for the run and refreshes it 60 s
// before expiry, so a long run does not start to receive 401 responses.
type TokenSource struct {
	cfg    *config.CitadelConfig
	client *http.Client

	// now supplies the current time. A test replaces it to move the clock
	// without a sleep.
	now func() time.Time

	mu     sync.Mutex
	token  string
	expiry time.Time
}

// AcquireToken performs the token acquisition of SDD section 4.2 phase 5.
// AcquireToken returns a source that keeps the token valid for the whole run.
// A configuration with no auth block for the app target gives a source that
// returns an empty token.
func AcquireToken(ctx context.Context, cfg *config.CitadelConfig, httpClient *http.Client) (*TokenSource, error) {
	source := &TokenSource{cfg: cfg, client: httpClient, now: time.Now}
	if _, err := source.Token(ctx); err != nil {
		return nil, err
	}
	return source, nil
}

// Token returns a bearer token that stays valid for at least the refresh
// margin. Token returns an empty string when the app target names no auth
// block.
func (t *TokenSource) Token(ctx context.Context) (string, error) {
	authName, authCfg, ok := t.authConfig()
	if !ok {
		return "", nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.token != "" && !t.needsRefresh() {
		return t.token, nil
	}

	token, lifetime, err := requestToken(ctx, authName, authCfg, t.client)
	if err != nil {
		return "", err
	}

	t.token = token
	if lifetime > 0 {
		t.expiry = t.now().Add(lifetime)
	} else {
		// A response without expires_in gives no expiry to work from, so the
		// token holds for the run rather than refreshing on every request.
		t.expiry = time.Time{}
	}
	return t.token, nil
}

// cached returns the token the source holds, without a request.
func (t *TokenSource) cached() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.token
}

// needsRefresh reports whether the cached token is inside the refresh margin.
// The caller holds the lock.
func (t *TokenSource) needsRefresh() bool {
	if t.expiry.IsZero() {
		return false
	}
	return !t.now().Before(t.expiry.Add(-refreshMargin))
}

// authConfig returns the auth block that the app target names.
func (t *TokenSource) authConfig() (string, config.AuthConfig, bool) {
	if t.cfg == nil || t.cfg.Targets.App == nil || t.cfg.Targets.App.Auth == "" {
		return "", config.AuthConfig{}, false
	}
	name := t.cfg.Targets.App.Auth
	authCfg, ok := t.cfg.Auth[name]
	if !ok {
		return "", config.AuthConfig{}, false
	}
	return name, authCfg, true
}

// requestToken performs an OAuth2 client-credentials POST to the tokenUrl of
// the named auth block. requestToken returns the token with the lifetime that
// expires_in reports, or a zero lifetime when the response omits it.
func requestToken(ctx context.Context, authName string, authCfg config.AuthConfig, httpClient *http.Client) (string, time.Duration, error) {
	if authCfg.TokenURL == "" {
		return "", 0, &InvalidInputError{Msg: fmt.Sprintf("auth %q has no tokenUrl", authName)}
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", authCfg.ClientID)
	form.Set("client_secret", authCfg.ClientSecret)
	if len(authCfg.Scopes) > 0 {
		form.Set("scope", strings.Join(authCfg.Scopes, " "))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, authCfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, &EnvironmentError{Msg: fmt.Sprintf("build token request for auth %q: %v", authName, err)}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", 0, &EnvironmentError{Msg: fmt.Sprintf("token request to %s failed: %v", authCfg.TokenURL, err)}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return "", 0, &EnvironmentError{Msg: fmt.Sprintf("token request returned status %d", resp.StatusCode)}
	}

	var tokenResp struct {
		AccessToken string  `json:"access_token"`
		ExpiresIn   float64 `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", 0, &EnvironmentError{Msg: fmt.Sprintf("decode token response: %v", err)}
	}

	if tokenResp.AccessToken == "" {
		return "", 0, &EnvironmentError{Msg: "token response has empty access_token"}
	}

	lifetime := time.Duration(tokenResp.ExpiresIn * float64(time.Second))
	return tokenResp.AccessToken, lifetime, nil
}
