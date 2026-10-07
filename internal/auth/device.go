package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type cacheFile struct {
	Entries map[string]Token `json:"entries"`
}

type Client struct {
	TenantID string
	ClientID string
	Scopes   []string
	CacheKey string
	Path     string
	HTTP     *http.Client
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) CachedToken() (string, error) {
	tok, err := c.read()
	if err != nil {
		return "", err
	}
	if tok.AccessToken == "" || time.Until(tok.ExpiresAt) <= time.Minute {
		return "", fmt.Errorf("cached token is missing or expired; run login")
	}
	return tok.AccessToken, nil
}

// Refresh returns a cached token, renewing it with the refresh token when it is near expiry.
// It never starts a device-code prompt.
func (c *Client) Refresh(ctx context.Context) (string, error) {
	tok, err := c.read()
	if err != nil {
		return "", err
	}
	if tok.AccessToken != "" && time.Until(tok.ExpiresAt) > time.Minute {
		return tok.AccessToken, nil
	}
	if tok.RefreshToken == "" {
		return "", fmt.Errorf("cached token is missing or expired; run login")
	}
	refreshed, err := c.refresh(ctx, tok.RefreshToken)
	if err != nil {
		return "", err
	}
	return refreshed.AccessToken, nil
}

// DeviceToken always starts a new device-code sign-in. Use it when the cached
// runtime token belongs to a different test user.
func (c *Client) DeviceToken(ctx context.Context) (string, error) {
	fresh, err := c.deviceCode(ctx)
	if err != nil {
		return "", err
	}
	return fresh.AccessToken, nil
}

func (c *Client) Snapshot() (Token, bool) {
	tok, err := c.read()
	if err != nil {
		return Token{}, false
	}
	return tok, true
}

func (c *Client) Restore(tok Token) error {
	return c.write(tok)
}

func (c *Client) Token(ctx context.Context) (string, error) {
	tok, err := c.read()
	if err == nil && time.Until(tok.ExpiresAt) > time.Minute && tok.AccessToken != "" {
		return tok.AccessToken, nil
	}
	if err == nil && tok.RefreshToken != "" {
		refreshed, refreshErr := c.refresh(ctx, tok.RefreshToken)
		if refreshErr == nil {
			return refreshed.AccessToken, nil
		}
	}
	fresh, err := c.deviceCode(ctx)
	if err != nil {
		return "", err
	}
	return fresh.AccessToken, nil
}

func (c *Client) deviceCode(ctx context.Context) (Token, error) {
	form := url.Values{}
	form.Set("client_id", c.ClientID)
	form.Set("scope", strings.Join(c.Scopes, " "))
	endpoint := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/devicecode", c.TenantID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return Token{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return Token{}, fmt.Errorf("device code request failed: %s", truncate(body))
	}
	var start struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		Message         string `json:"message"`
		Interval        int    `json:"interval"`
		ExpiresIn       int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &start); err != nil {
		return Token{}, err
	}
	if start.Interval == 0 {
		start.Interval = 5
	}
	fmt.Fprintln(os.Stderr, start.Message)
	fmt.Fprintf(os.Stderr, "Sign in at %s and enter code %s. Do not paste a password into the agent chat.\n", start.VerificationURI, start.UserCode)

	deadline := time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return Token{}, ctx.Err()
		case <-time.After(time.Duration(start.Interval) * time.Second):
		}
		tok, pending, err := c.poll(ctx, start.DeviceCode)
		if err != nil {
			return Token{}, err
		}
		if !pending {
			if err := c.write(tok); err != nil {
				return Token{}, err
			}
			return tok, nil
		}
	}
	return Token{}, fmt.Errorf("device code expired before sign-in completed")
}

func (c *Client) poll(ctx context.Context, deviceCode string) (Token, bool, error) {
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	form.Set("client_id", c.ClientID)
	form.Set("device_code", deviceCode)
	return c.tokenRequest(ctx, form)
}

func (c *Client) refresh(ctx context.Context, refreshToken string) (Token, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", c.ClientID)
	form.Set("refresh_token", refreshToken)
	form.Set("scope", strings.Join(c.Scopes, " "))
	tok, pending, err := c.tokenRequest(ctx, form)
	if err != nil {
		return Token{}, err
	}
	if pending {
		return Token{}, fmt.Errorf("refresh still pending")
	}
	if tok.RefreshToken == "" {
		tok.RefreshToken = refreshToken
	}
	if err := c.write(tok); err != nil {
		return Token{}, err
	}
	return tok, nil
}

func (c *Client) tokenRequest(ctx context.Context, form url.Values) (Token, bool, error) {
	endpoint := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", c.TenantID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return Token{}, false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var raw struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Token{}, false, err
	}
	if raw.Error == "authorization_pending" || raw.Error == "slow_down" {
		return Token{}, true, nil
	}
	if raw.Error != "" || resp.StatusCode >= 300 {
		return Token{}, false, fmt.Errorf("token request failed: %s", truncate(body))
	}
	return Token{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(raw.ExpiresIn) * time.Second),
	}, false, nil
}

func (c *Client) read() (Token, error) {
	b, err := os.ReadFile(c.Path)
	if err != nil {
		return Token{}, err
	}
	var file cacheFile
	if err := json.Unmarshal(b, &file); err != nil {
		return Token{}, err
	}
	tok, ok := file.Entries[c.CacheKey]
	if !ok {
		return Token{}, os.ErrNotExist
	}
	return tok, nil
}

func (c *Client) write(tok Token) error {
	file := cacheFile{Entries: map[string]Token{}}
	if b, err := os.ReadFile(c.Path); err == nil {
		_ = json.Unmarshal(b, &file)
	}
	if file.Entries == nil {
		file.Entries = map[string]Token{}
	}
	file.Entries[c.CacheKey] = tok
	b, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.Path, b, 0o600)
}

func truncate(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 500 {
		return s[:500]
	}
	return s
}
