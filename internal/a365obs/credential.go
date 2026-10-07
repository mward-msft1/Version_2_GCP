package a365obs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mward-msft1/Version_2_GCP/internal/auth"
	"github.com/mward-msft1/Version_2_GCP/internal/config"
)

const observabilityAppID = "9b975845-388f-4429-889e-eab1ef63949c"

type credential struct {
	KeyID          string    `json:"keyId"`
	SecretText     string    `json:"secretText"`
	BlueprintAppID string    `json:"blueprintAppId"`
	CreatedAt      time.Time `json:"createdAt"`
}

func ensureSecret(ctx context.Context, cfg config.Config) (string, error) {
	if secret := strings.TrimSpace(os.Getenv("A365_BLUEPRINT_SECRET")); secret != "" {
		return secret, nil
	}
	path, err := credentialPath()
	if err != nil {
		return "", err
	}
	if existing, ok := readCredential(path, cfg.BlueprintID); ok {
		return existing.SecretText, nil
	}
	token, err := adminToken(ctx, cfg)
	if err != nil {
		return "", err
	}
	created, err := addPassword(ctx, token, cfg.BlueprintID)
	if err != nil {
		return "", err
	}
	created.BlueprintAppID = cfg.BlueprintID
	created.CreatedAt = time.Now().UTC()
	if err := writeCredential(path, created); err != nil {
		return "", err
	}
	return created.SecretText, nil
}

func adminToken(ctx context.Context, cfg config.Config) (string, error) {
	path, err := cfg.TokenPath()
	if err != nil {
		return "", err
	}
	return (&auth.Client{
		TenantID: cfg.TenantID,
		ClientID: config.BootstrapClientID,
		Scopes: []string{
			"AgentIdentityBlueprint.Create",
			"AgentIdentityBlueprint.ReadWrite.All",
			"AgentIdentityBlueprintPrincipal.Create",
			"Application.ReadWrite.All",
			"DelegatedPermissionGrant.ReadWrite.All",
			"User.Read",
			"AgentRegistration.ReadWrite.All",
			"offline_access",
		},
		CacheKey: "register-caldova",
		Path:     path,
	}).Refresh(ctx)
}

func addPassword(ctx context.Context, token, blueprintID string) (credential, error) {
	body, _ := json.Marshal(map[string]any{
		"passwordCredential": map[string]string{
			"displayName":   "a365-otel",
			"startDateTime": time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339),
		},
	})
	endpoint := "https://graph.microsoft.com/v1.0/applications/" + blueprintID + "/microsoft.graph.agentIdentityBlueprint/addPassword"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return credential{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return credential{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return credential{}, fmt.Errorf("add blueprint credential returned %d: %s", resp.StatusCode, truncate(string(raw), 400))
	}
	var created credential
	if err := json.Unmarshal(raw, &created); err != nil {
		return credential{}, err
	}
	if created.SecretText == "" {
		return credential{}, fmt.Errorf("blueprint credential response did not include a secret")
	}
	return created, nil
}

func observabilityTokenReady(ctx context.Context, cfg config.Config) (string, error) {
	secret, err := ensureSecret(ctx, cfg)
	if err != nil {
		return "", err
	}
	token, err := observabilityToken(ctx, cfg, secret)
	if err == nil || !strings.Contains(err.Error(), "AADSTS7000215") {
		return token, err
	}
	// A new blueprint secret is often rejected until Entra replicates it.
	for attempt := 1; attempt <= 4; attempt++ {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(8 * time.Second):
		}
		token, err = observabilityToken(ctx, cfg, secret)
		if err == nil || !strings.Contains(err.Error(), "AADSTS7000215") {
			return token, err
		}
	}
	path, pathErr := credentialPath()
	if pathErr != nil {
		return "", err
	}
	_ = os.Remove(path)
	secret, err = ensureSecret(ctx, cfg)
	if err != nil {
		return "", err
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(15 * time.Second):
	}
	return observabilityToken(ctx, cfg, secret)
}

func observabilityToken(ctx context.Context, cfg config.Config, secret string) (string, error) {
	exchange, err := tokenRequest(ctx, cfg.TenantID, url.Values{
		"client_id":     {cfg.BlueprintID},
		"scope":         {"api://AzureADTokenExchange/.default"},
		"grant_type":    {"client_credentials"},
		"client_secret": {secret},
		"fmi_path":      {cfg.AgentIdentityID},
	})
	if err != nil {
		return "", fmt.Errorf("agent identity exchange token: %w", err)
	}
	access, err := tokenRequest(ctx, cfg.TenantID, url.Values{
		"client_id":             {cfg.AgentIdentityID},
		"scope":                 {observabilityAppID + "/.default"},
		"grant_type":            {"client_credentials"},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {exchange},
	})
	if err != nil {
		return "", fmt.Errorf("agent 365 observability token: %w", err)
	}
	return access, nil
}

func tokenRequest(ctx context.Context, tenantID string, form url.Values) (string, error) {
	endpoint := "https://login.microsoftonline.com/" + tenantID + "/oauth2/v2.0/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var parsed struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", err
	}
	if parsed.AccessToken == "" {
		return "", fmt.Errorf("%s: %s", first(parsed.Error, resp.Status), truncate(parsed.Description, 300))
	}
	return parsed.AccessToken, nil
}

func LookupUserID(ctx context.Context, cfg config.Config, userPrincipalName string) (string, error) {
	token, err := adminToken(ctx, cfg)
	if err != nil {
		return "", err
	}
	endpoint := "https://graph.microsoft.com/v1.0/users/" + url.PathEscape(userPrincipalName) + "?$select=id"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var parsed struct {
		ID    string `json:"id"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", err
	}
	if parsed.ID == "" {
		return "", fmt.Errorf("lookup %s: %s", userPrincipalName, first(parsed.Error.Message, resp.Status))
	}
	return parsed.ID, nil
}

func credentialPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "caldova-gcp-agent")
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(path, "blueprint-credential.json"), nil
}

func readCredential(path, blueprintID string) (credential, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return credential{}, false
	}
	var saved credential
	if json.Unmarshal(raw, &saved) != nil || saved.SecretText == "" || !strings.EqualFold(saved.BlueprintAppID, blueprintID) {
		return credential{}, false
	}
	return saved, true
}

func writeCredential(path string, saved credential) error {
	raw, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}
