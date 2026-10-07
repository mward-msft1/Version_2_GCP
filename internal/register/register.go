package register

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/mward-msft1/Version_2_GCP/internal/auth"
	"github.com/mward-msft1/Version_2_GCP/internal/config"
)

var registerScopes = []string{
	"AgentIdentityBlueprint.Create",
	"AgentIdentityBlueprint.ReadWrite.All",
	"AgentIdentityBlueprintPrincipal.Create",
	"Application.ReadWrite.All",
	"DelegatedPermissionGrant.ReadWrite.All",
	"User.Read",
	"AgentRegistration.ReadWrite.All",
	"offline_access",
}

var runtimeScopes = []string{
	"User.Read",
	"Sites.Read.All",
	"Files.Read.All",
	"Mail.Send",
	"Chat.ReadWrite",
	"ChannelMessage.Send",
	"Team.ReadBasic.All",
	"Channel.ReadBasic.All",
	"Content.Process.User",
	"ContentActivity.Write",
	"ProtectionScopes.Compute.User",
	"offline_access",
}

type Result struct {
	BlueprintAppID      string   `json:"blueprintAppId"`
	BlueprintID         string   `json:"blueprintId"`
	PrincipalID         string   `json:"principalId,omitempty"`
	AgentIdentityID     string   `json:"agentIdentityId,omitempty"`
	AgentRegistrationID string   `json:"agentRegistrationId,omitempty"`
	SignedInUser        string   `json:"signedInUser,omitempty"`
	ClientID            string   `json:"clientId"`
	RuntimeClientID     string   `json:"runtimeClientId,omitempty"`
	TenantID            string   `json:"tenantId"`
	AdminConsentURL     string   `json:"adminConsentUrl"`
	Warnings            []string `json:"warnings,omitempty"`
}

func Run(ctx context.Context, cfg config.Config) (Result, error) {
	if err := requireCaldovaTenant(cfg.TenantID); err != nil {
		return Result{}, err
	}
	path, err := cfg.TokenPath()
	if err != nil {
		return Result{}, err
	}
	token, err := (&auth.Client{
		TenantID: cfg.TenantID,
		ClientID: config.BootstrapClientID,
		Scopes:   registerScopes,
		CacheKey: "register-caldova",
		Path:     path,
	}).Token(ctx)
	if err != nil {
		return Result{}, err
	}
	signedIn, err := requireCaldovaToken(token)
	if err != nil {
		return Result{}, err
	}
	fmt.Fprintf(os.Stderr, "Caldova sign-in accepted for %s. Corporate tenant tokens are rejected.\n", signedIn)
	sponsorID, err := graphGetID(ctx, token, "https://graph.microsoft.com/v1.0/me?$select=id")
	if err != nil {
		return Result{}, fmt.Errorf("read signed-in user: %w", err)
	}
	if cfg.ClientID == "" {
		if existing, err := existingBlueprintAppID(ctx, token, cfg.AgentName); err != nil {
			return Result{}, err
		} else if existing != "" {
			cfg.ClientID = existing
		}
	}
	if cfg.ClientID != "" {
		return repair(ctx, cfg, token, sponsorID, signedIn)
	}
	blueprint, err := graphJSON(ctx, token, http.MethodPost, "https://graph.microsoft.com/v1.0/applications/microsoft.graph.agentIdentityBlueprint", map[string]any{
		"@odata.type": "Microsoft.Graph.AgentIdentityBlueprint",
		"displayName": cfg.AgentName,
		"description": cfg.Description,
		"sponsors@odata.bind": []string{
			"https://graph.microsoft.com/v1.0/users/" + sponsorID,
		},
		"owners@odata.bind": []string{
			"https://graph.microsoft.com/v1.0/users/" + sponsorID,
		},
	})
	if err != nil {
		return Result{}, fmt.Errorf("create agent identity blueprint: %w", err)
	}
	result := Result{
		SignedInUser:   signedIn,
		BlueprintAppID: str(blueprint, "appId"),
		BlueprintID:    str(blueprint, "id"),
		TenantID:       cfg.TenantID,
		ClientID:       str(blueprint, "appId"),
	}
	access, missing, err := requiredAccess(ctx, token)
	if err != nil {
		result.Warnings = append(result.Warnings, "could not resolve Graph permission IDs: "+err.Error())
	} else {
		if len(missing) > 0 {
			result.Warnings = append(result.Warnings, "Graph scopes not published on the Caldova service principal: "+strings.Join(missing, ", "))
		}
		if patchErr := configureBlueprint(ctx, token, result.BlueprintID, cfg, access); patchErr != nil {
			result.Warnings = append(result.Warnings, patchErr.Error())
		}
	}
	principal, err := graphJSON(ctx, token, http.MethodPost, "https://graph.microsoft.com/v1.0/servicePrincipals/microsoft.graph.agentIdentityBlueprintPrincipal", map[string]any{
		"appId": result.BlueprintAppID,
	})
	if err != nil {
		result.Warnings = append(result.Warnings, "blueprint principal was not created: "+err.Error())
	} else {
		result.PrincipalID = str(principal, "id")
	}
	identity, err := graphJSON(ctx, token, http.MethodPost, "https://graph.microsoft.com/v1.0/servicePrincipals/microsoft.graph.agentIdentity", map[string]any{
		"displayName":              cfg.AgentName,
		"agentIdentityBlueprintId": result.BlueprintAppID,
		"sponsors@odata.bind": []string{
			"https://graph.microsoft.com/v1.0/users/" + sponsorID,
		},
	})
	if err != nil {
		result.Warnings = append(result.Warnings, "agent identity was not created: "+err.Error())
	} else {
		result.AgentIdentityID = str(identity, "id")
		if appID := str(identity, "appId"); appID != "" {
			result.AgentIdentityID = appID
		}
	}
	if err := registerAgent365(ctx, token, sponsorID, &result, cfg); err != nil {
		result.Warnings = append(result.Warnings, err.Error())
	}
	result.AdminConsentURL = fmt.Sprintf("https://login.microsoftonline.com/%s/adminconsent?client_id=%s", cfg.TenantID, result.ClientID)
	if err := writeGenerated(cfg, result); err != nil {
		return result, err
	}
	return result, nil
}

type LiveStatus struct {
	TenantID            string   `json:"tenantId"`
	SignedInUser        string   `json:"signedInUser"`
	BlueprintID         string   `json:"blueprintId"`
	BlueprintAppID      string   `json:"blueprintAppId"`
	DisplayName         string   `json:"displayName"`
	PrincipalID         string   `json:"principalId"`
	AgentIdentityID     string   `json:"agentIdentityId"`
	AgentRegistrationID string   `json:"agentRegistrationId"`
	DeclaredScopes      []string `json:"declaredScopes"`
	InheritableGraph    bool     `json:"inheritableGraph"`
	AdminConsentGranted bool     `json:"adminConsentGranted"`
	ConsentScopes       string   `json:"consentScopes,omitempty"`
	DSPMApplicationID   string   `json:"dspmApplicationId"`
	RuntimeClientID     string   `json:"runtimeClientId,omitempty"`
	RuntimeConsent      bool     `json:"runtimeConsent"`
	Warnings            []string `json:"warnings,omitempty"`
}

func Verify(ctx context.Context, cfg config.Config) (LiveStatus, error) {
	if err := requireCaldovaTenant(cfg.TenantID); err != nil {
		return LiveStatus{}, err
	}
	if cfg.ClientID == "" {
		return LiveStatus{}, fmt.Errorf("no Caldova client id; run register first")
	}
	path, err := cfg.TokenPath()
	if err != nil {
		return LiveStatus{}, err
	}
	token, err := (&auth.Client{
		TenantID: cfg.TenantID,
		ClientID: config.BootstrapClientID,
		Scopes:   registerScopes,
		CacheKey: "register-caldova",
		Path:     path,
	}).CachedToken()
	if err != nil {
		return LiveStatus{}, fmt.Errorf("no cached Caldova admin token; run register again: %w", err)
	}
	signedIn, err := requireCaldovaToken(token)
	if err != nil {
		return LiveStatus{}, err
	}
	status := LiveStatus{
		TenantID:          cfg.TenantID,
		SignedInUser:      signedIn,
		BlueprintAppID:    cfg.ClientID,
		DSPMApplicationID: cfg.RuntimeClientID,
		RuntimeClientID:   cfg.RuntimeClientID,
	}
	if status.DSPMApplicationID == "" {
		status.DSPMApplicationID = cfg.ClientID
	}
	var blueprint map[string]any
	if err := graphInto(ctx, token, http.MethodGet, "https://graph.microsoft.com/v1.0/applications/microsoft.graph.agentIdentityBlueprint/"+url.PathEscape(cfg.ClientID)+"?$select=id,appId,displayName,requiredResourceAccess", nil, &blueprint); err != nil {
		status.Warnings = append(status.Warnings, "blueprint read failed: "+err.Error())
	} else {
		status.BlueprintID = str(blueprint, "id")
		status.BlueprintAppID = str(blueprint, "appId")
		status.DisplayName = str(blueprint, "displayName")
		status.DeclaredScopes = scopeNames(blueprint["requiredResourceAccess"])
	}
	var inherit struct {
		Value []struct {
			ResourceAppID string `json:"resourceAppId"`
		} `json:"value"`
	}
	if err := graphInto(ctx, token, http.MethodGet, "https://graph.microsoft.com/v1.0/applications/microsoft.graph.agentIdentityBlueprint/"+url.PathEscape(cfg.ClientID)+"/inheritablePermissions", nil, &inherit); err != nil {
		status.Warnings = append(status.Warnings, "inheritable permissions read failed: "+err.Error())
	} else {
		for _, item := range inherit.Value {
			if strings.EqualFold(item.ResourceAppID, config.GraphAppID) {
				status.InheritableGraph = true
			}
		}
	}
	var principals struct {
		Value []struct {
			ID    string `json:"id"`
			AppID string `json:"appId"`
		} `json:"value"`
	}
	filter := url.QueryEscape("appId eq '" + cfg.ClientID + "'")
	if err := graphInto(ctx, token, http.MethodGet, "https://graph.microsoft.com/v1.0/servicePrincipals?$filter="+filter+"&$select=id,appId", nil, &principals); err != nil {
		status.Warnings = append(status.Warnings, "principal lookup failed: "+err.Error())
	} else if len(principals.Value) > 0 {
		status.PrincipalID = principals.Value[0].ID
		var grants struct {
			Value []struct {
				Scope       string `json:"scope"`
				ConsentType string `json:"consentType"`
			} `json:"value"`
		}
		grantURL := "https://graph.microsoft.com/v1.0/oauth2PermissionGrants?$filter=" + url.QueryEscape("clientId eq '"+status.PrincipalID+"'")
		if err := graphInto(ctx, token, http.MethodGet, grantURL, nil, &grants); err != nil {
			status.Warnings = append(status.Warnings, "consent lookup failed: "+err.Error())
		} else {
			for _, grant := range grants.Value {
				if strings.EqualFold(grant.ConsentType, "AllPrincipals") && strings.TrimSpace(grant.Scope) != "" {
					status.AdminConsentGranted = true
					status.ConsentScopes = strings.TrimSpace(grant.Scope)
				}
			}
		}
	}
	if gen, err := os.ReadFile(cfg.GeneratedPath); err == nil {
		var saved struct {
			AgentIdentityID string `json:"agentIdentityId"`
		}
		_ = json.Unmarshal(gen, &saved)
		if saved.AgentIdentityID != "" {
			var identity map[string]any
			if err := graphInto(ctx, token, http.MethodGet, "https://graph.microsoft.com/v1.0/servicePrincipals/"+url.PathEscape(saved.AgentIdentityID)+"?$select=id,displayName,servicePrincipalType", nil, &identity); err != nil {
				status.Warnings = append(status.Warnings, "agent identity read failed: "+err.Error())
			} else {
				status.AgentIdentityID = str(identity, "id")
			}
		}
	}
	var registration map[string]any
	if err := graphInto(ctx, token, http.MethodGet, "https://graph.microsoft.com/beta/copilot/agentRegistrations/"+url.PathEscape(config.AgentID), nil, &registration); err != nil {
		status.Warnings = append(status.Warnings, "Agent 365 registry read failed: "+err.Error())
	} else {
		status.AgentRegistrationID = str(registration, "id")
	}
	if cfg.RuntimeClientID != "" {
		var runtimeSP struct {
			Value []struct {
				ID string `json:"id"`
			} `json:"value"`
		}
		filter := url.QueryEscape("appId eq '" + cfg.RuntimeClientID + "'")
		if err := graphInto(ctx, token, http.MethodGet, "https://graph.microsoft.com/v1.0/servicePrincipals?$filter="+filter+"&$select=id", nil, &runtimeSP); err != nil {
			status.Warnings = append(status.Warnings, "runtime client lookup failed: "+err.Error())
		} else if len(runtimeSP.Value) > 0 {
			var grants struct {
				Value []struct {
					Scope       string `json:"scope"`
					ConsentType string `json:"consentType"`
				} `json:"value"`
			}
			grantURL := "https://graph.microsoft.com/v1.0/oauth2PermissionGrants?$filter=" + url.QueryEscape("clientId eq '"+runtimeSP.Value[0].ID+"'")
			if err := graphInto(ctx, token, http.MethodGet, grantURL, nil, &grants); err != nil {
				status.Warnings = append(status.Warnings, "runtime consent lookup failed: "+err.Error())
			} else {
				for _, grant := range grants.Value {
					if strings.EqualFold(grant.ConsentType, "AllPrincipals") && strings.Contains(grant.Scope, "Mail.Send") {
						status.RuntimeConsent = true
					}
				}
			}
		}
	}
	return status, nil
}

func scopeNames(raw any) []string {
	items, _ := raw.([]any)
	var names []string
	for _, item := range items {
		entry, _ := item.(map[string]any)
		access, _ := entry["resourceAccess"].([]any)
		names = append(names, fmt.Sprintf("%s:%d", str(entry, "resourceAppId"), len(access)))
	}
	return names
}

func RuntimeAuth(cfg config.Config) (*auth.Client, error) {
	clientID := cfg.RuntimeClientID
	if clientID == "" {
		return nil, fmt.Errorf("runtime public client is missing; run register again in the Caldova tenant")
	}
	path, err := cfg.TokenPath()
	if err != nil {
		return nil, err
	}
	return &auth.Client{
		TenantID: cfg.TenantID,
		ClientID: clientID,
		Scopes:   runtimeScopes,
		CacheKey: "runtime:" + clientID,
		Path:     path,
	}, nil
}

func requiredAccess(ctx context.Context, token string) (map[string]any, []string, error) {
	var sp struct {
		Value []struct {
			AppID  string `json:"appId"`
			Scopes []struct {
				ID    string `json:"id"`
				Value string `json:"value"`
				Type  string `json:"type"`
			} `json:"oauth2PermissionScopes"`
		} `json:"value"`
	}
	endpoint := "https://graph.microsoft.com/v1.0/servicePrincipals?$filter=" + url.QueryEscape("appId eq '"+config.GraphAppID+"'") + "&$select=appId,oauth2PermissionScopes"
	if err := graphInto(ctx, token, http.MethodGet, endpoint, nil, &sp); err != nil {
		return nil, nil, err
	}
	var beta struct {
		Value []struct {
			AppID  string `json:"appId"`
			Scopes []struct {
				ID    string `json:"id"`
				Value string `json:"value"`
				Type  string `json:"type"`
			} `json:"oauth2PermissionScopes"`
		} `json:"value"`
	}
	betaEndpoint := "https://graph.microsoft.com/beta/servicePrincipals?$filter=" + url.QueryEscape("appId eq '"+config.GraphAppID+"'") + "&$select=appId,oauth2PermissionScopes"
	if err := graphInto(ctx, token, http.MethodGet, betaEndpoint, nil, &beta); err == nil && len(beta.Value) > 0 {
		if len(sp.Value) == 0 {
			sp.Value = beta.Value
		} else {
			sp.Value[0].Scopes = append(sp.Value[0].Scopes, beta.Value[0].Scopes...)
		}
	}
	if len(sp.Value) == 0 {
		return nil, nil, fmt.Errorf("Microsoft Graph service principal was not found")
	}
	want := []string{"User.Read", "Sites.Read.All", "Files.Read.All", "Mail.Send", "Chat.ReadWrite", "ChannelMessage.Send", "Team.ReadBasic.All", "Channel.ReadBasic.All", "Content.Process.User", "ContentActivity.Write", "ProtectionScopes.Compute.User"}
	var scopes []any
	var missing []string
	for _, name := range want {
		found := false
		for _, scope := range sp.Value[0].Scopes {
			if !strings.EqualFold(scope.Value, name) {
				continue
			}
			// Purview records these delegated scopes as Admin on the Graph
			// service principal, but they are granted on oauth2PermissionGrants.
			if strings.EqualFold(scope.Type, "User") || isDelegatedPurviewScope(name) {
				scopes = append(scopes, map[string]any{"id": scope.ID, "type": "Scope"})
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, name)
		}
	}
	if len(scopes) == 0 {
		return nil, missing, fmt.Errorf("no delegated Graph scopes matched")
	}
	return map[string]any{
		"resourceAppId":  config.GraphAppID,
		"resourceAccess": scopes,
	}, missing, nil
}

func isDelegatedPurviewScope(name string) bool {
	switch strings.ToLower(name) {
	case "content.process.user", "contentactivity.write", "protectionscopes.compute.user":
		return true
	default:
		return false
	}
}

func writeGenerated(cfg config.Config, result Result) error {
	payload := map[string]any{
		"agentName":            cfg.AgentName,
		"tenantId":             cfg.TenantID,
		"gcpProjectNumber":     cfg.GCPProjectNumber,
		"clientId":             result.ClientID,
		"runtimeClientId":      result.RuntimeClientID,
		"blueprintAppId":       result.BlueprintAppID,
		"blueprintId":          result.BlueprintID,
		"principalId":          result.PrincipalID,
		"agentIdentityId":      result.AgentIdentityID,
		"agentRegistrationId":  result.AgentRegistrationID,
		"signedInUser":         result.SignedInUser,
		"registrationTenantId": config.TenantID,
		"adminConsentUrl":      result.AdminConsentURL,
		"warnings":             result.Warnings,
		"dspm": map[string]string{
			"applicationId":    result.RuntimeClientID,
			"agentBlueprintId": result.BlueprintAppID,
			"note":             "DSPM sees the runtime public client that calls processContent. Scope the Purview collection policy to that application ID. The Agent 365 blueprint remains the agent identity.",
		},
		"vertexSync": "After adkgo deploy, connect Google Vertex AI in Microsoft 365 admin center > Agents > Connected platforms and sync project " + cfg.GCPProjectNumber + " in " + cfg.Location + ".",
	}
	if previous, err := os.ReadFile(cfg.GeneratedPath); err == nil {
		var existing map[string]any
		if json.Unmarshal(previous, &existing) == nil {
			for _, key := range []string{"reasoningEngine", "gcpProjectId", "agentEngineId"} {
				if _, ok := payload[key]; !ok && existing[key] != nil {
					payload[key] = existing[key]
				}
			}
		}
	}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cfg.GeneratedPath, b, 0o600)
}

func repair(ctx context.Context, cfg config.Config, token, sponsorID, signedIn string) (Result, error) {
	result := Result{
		SignedInUser:   signedIn,
		BlueprintAppID: cfg.ClientID,
		ClientID:       cfg.ClientID,
		TenantID:       cfg.TenantID,
		BlueprintID:    cfg.ClientID,
	}
	if gen, err := os.ReadFile(cfg.GeneratedPath); err == nil {
		_ = json.Unmarshal(gen, &result)
		result.SignedInUser = signedIn
		result.TenantID = cfg.TenantID
		result.Warnings = nil
	}
	if id, err := applicationObjectID(ctx, token, cfg.ClientID); err == nil && id != "" {
		result.BlueprintID = id
	}
	access, missing, err := requiredAccess(ctx, token)
	if err != nil {
		result.Warnings = append(result.Warnings, "could not resolve Graph permission IDs: "+err.Error())
	} else {
		if len(missing) > 0 {
			result.Warnings = append(result.Warnings, "Graph scopes not published on the Caldova service principal: "+strings.Join(missing, ", "))
		}
		if err := configureBlueprint(ctx, token, result.BlueprintID, cfg, access); err != nil {
			result.Warnings = append(result.Warnings, err.Error())
		}
	}
	if err := registerAgent365(ctx, token, sponsorID, &result, cfg); err != nil {
		result.Warnings = append(result.Warnings, err.Error())
	}
	if result.PrincipalID != "" {
		if err := grantAdminConsent(ctx, token, result.PrincipalID); err != nil && !strings.Contains(strings.ToLower(err.Error()), "already exists") {
			result.Warnings = append(result.Warnings, "admin consent was not granted through Graph: "+err.Error())
		}
	}
	runtimeID, runtimeErr := ensureRuntimeClient(ctx, token, cfg, result.RuntimeClientID, access)
	if runtimeID != "" {
		result.RuntimeClientID = runtimeID
	}
	if runtimeErr != nil {
		result.Warnings = append(result.Warnings, "runtime public client was not ready: "+runtimeErr.Error())
	}
	result.AdminConsentURL = fmt.Sprintf("https://login.microsoftonline.com/%s/adminconsent?client_id=%s", cfg.TenantID, result.ClientID)
	if err := writeGenerated(cfg, result); err != nil {
		return result, err
	}
	return result, nil
}

func configureBlueprint(ctx context.Context, token, blueprintID string, cfg config.Config, access map[string]any) error {
	_, err := graphJSON(ctx, token, http.MethodPatch, "https://graph.microsoft.com/v1.0/applications/microsoft.graph.agentIdentityBlueprint/"+url.PathEscape(blueprintID), map[string]any{
		"requiredResourceAccess": []any{access},
	})
	if err != nil {
		return fmt.Errorf("blueprint permission update failed: %w", err)
	}
	_ = cfg
	_, inheritErr := graphJSON(ctx, token, http.MethodPost, "https://graph.microsoft.com/v1.0/applications/microsoft.graph.agentIdentityBlueprint/"+url.PathEscape(blueprintID)+"/inheritablePermissions", map[string]any{
		"resourceAppId": config.GraphAppID,
		"inheritableScopes": map[string]any{
			"@odata.type": "#microsoft.graph.allAllowedScopes",
			"kind":        "allAllowed",
		},
		"inheritableRoles": map[string]any{
			"@odata.type": "#microsoft.graph.noRoles",
			"kind":        "none",
		},
	})
	if inheritErr != nil && !strings.Contains(inheritErr.Error(), "409") && !strings.Contains(strings.ToLower(inheritErr.Error()), "already exists") {
		return fmt.Errorf("inheritable Graph scopes were not set: %w", inheritErr)
	}
	return nil
}

func ensureRuntimeClient(ctx context.Context, token string, cfg config.Config, existing string, access map[string]any) (string, error) {
	if existing != "" {
		objectID, err := applicationObjectID(ctx, token, existing)
		if err == nil && objectID != "" {
			if access != nil {
				if _, err := graphJSON(ctx, token, http.MethodPatch, "https://graph.microsoft.com/v1.0/applications/"+url.PathEscape(objectID), map[string]any{
					"requiredResourceAccess": []any{access},
				}); err != nil {
					return existing, fmt.Errorf("runtime permission update failed: %w", err)
				}
			}
			var principals struct {
				Value []struct {
					ID string `json:"id"`
				} `json:"value"`
			}
			spEndpoint := "https://graph.microsoft.com/v1.0/servicePrincipals?$filter=" + url.QueryEscape("appId eq '"+existing+"'") + "&$select=id"
			if err := graphInto(ctx, token, http.MethodGet, spEndpoint, nil, &principals); err != nil || len(principals.Value) == 0 {
				return existing, fmt.Errorf("runtime service principal was not found")
			}
			if err := grantAdminConsent(ctx, token, principals.Value[0].ID); err != nil {
				return existing, fmt.Errorf("runtime admin consent was not updated: %w", err)
			}
			return existing, nil
		}
	}
	body := map[string]any{
		"displayName":            cfg.AgentName + " Runtime",
		"signInAudience":         "AzureADMyOrg",
		"isFallbackPublicClient": true,
		"publicClient": map[string]any{
			"redirectUris": []string{"http://localhost"},
		},
		"notes": "Caldova-only delegated runtime for " + cfg.AgentName + ". Agent 365 blueprint " + cfg.ClientID + ".",
	}
	if access != nil {
		body["requiredResourceAccess"] = []any{access}
	}
	created, err := graphJSON(ctx, token, http.MethodPost, "https://graph.microsoft.com/v1.0/applications", body)
	if err != nil {
		return "", err
	}
	appID := str(created, "appId")
	if appID == "" {
		return "", fmt.Errorf("runtime application did not return an appId")
	}
	principal, err := graphJSON(ctx, token, http.MethodPost, "https://graph.microsoft.com/v1.0/servicePrincipals", map[string]any{"appId": appID})
	if err != nil {
		return appID, fmt.Errorf("runtime service principal was not created: %w", err)
	}
	if err := grantAdminConsent(ctx, token, str(principal, "id")); err != nil {
		return appID, fmt.Errorf("runtime admin consent was not granted: %w", err)
	}
	return appID, nil
}

func grantAdminConsent(ctx context.Context, token, principalID string) error {
	var graphSP struct {
		Value []struct {
			ID string `json:"id"`
		} `json:"value"`
	}
	endpoint := "https://graph.microsoft.com/v1.0/servicePrincipals?$filter=" + url.QueryEscape("appId eq '"+config.GraphAppID+"'") + "&$select=id"
	if err := graphInto(ctx, token, http.MethodGet, endpoint, nil, &graphSP); err != nil {
		return err
	}
	if len(graphSP.Value) == 0 {
		return fmt.Errorf("Microsoft Graph service principal was not found")
	}
	scope := strings.Join(runtimeScopes[:len(runtimeScopes)-1], " ")
	_, err := graphJSON(ctx, token, http.MethodPost, "https://graph.microsoft.com/v1.0/oauth2PermissionGrants", map[string]any{
		"clientId":    principalID,
		"consentType": "AllPrincipals",
		"resourceId":  graphSP.Value[0].ID,
		"scope":       scope,
	})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return err
	}
	var grants struct {
		Value []struct {
			ID         string `json:"id"`
			ClientID   string `json:"clientId"`
			ResourceID string `json:"resourceId"`
		} `json:"value"`
	}
	list := "https://graph.microsoft.com/v1.0/oauth2PermissionGrants?$filter=" + url.QueryEscape("clientId eq '"+principalID+"'")
	if listErr := graphInto(ctx, token, http.MethodGet, list, nil, &grants); listErr != nil {
		return err
	}
	for _, grant := range grants.Value {
		if !strings.EqualFold(grant.ResourceID, graphSP.Value[0].ID) {
			continue
		}
		_, patchErr := graphJSON(ctx, token, http.MethodPatch, "https://graph.microsoft.com/v1.0/oauth2PermissionGrants/"+url.PathEscape(grant.ID), map[string]any{
			"scope": scope,
		})
		return patchErr
	}
	return err
}

func registerAgent365(ctx context.Context, token, sponsorID string, result *Result, cfg config.Config) error {
	if result.AgentRegistrationID != "" {
		var existing map[string]any
		err := graphInto(ctx, token, http.MethodGet, "https://graph.microsoft.com/beta/copilot/agentRegistrations/"+url.PathEscape(result.AgentRegistrationID), nil, &existing)
		if err == nil && str(existing, "id") != "" {
			return publishAgentCard(ctx, token, result.AgentRegistrationID, cfg)
		}
		result.AgentRegistrationID = ""
	}
	now := time.Now().UTC().Format(time.RFC3339)
	registration, err := graphJSON(ctx, token, http.MethodPost, "https://graph.microsoft.com/beta/copilot/agentRegistrations", map[string]any{
		"displayName":                cfg.AgentName,
		"description":                cfg.Description,
		"createdBy":                  sponsorID,
		"ownerIds":                   []string{sponsorID},
		"sourceAgentId":              cfg.AgentID,
		"originatingStore":           "Google Vertex",
		"agentIdentityBlueprintId":   result.BlueprintAppID,
		"agentIdentityId":            result.AgentIdentityID,
		"sourceCreatedDateTime":      now,
		"sourceLastModifiedDateTime": now,
	})
	if err != nil {
		return fmt.Errorf("Agent 365 registry entry was not created: %w", err)
	}
	result.AgentRegistrationID = str(registration, "id")
	if err := publishAgentCard(ctx, token, result.AgentRegistrationID, cfg); err != nil {
		return err
	}
	return nil
}

// Sync republishes the Agent 365 card and repairs the blueprint Purview consent
// without creating another blueprint or identity.
func Sync(ctx context.Context, cfg config.Config) error {
	if cfg.ClientID == "" || cfg.AgentIdentityID == "" {
		return fmt.Errorf("generated registration is incomplete; run register before sync")
	}
	path, err := cfg.TokenPath()
	if err != nil {
		return err
	}
	token, err := (&auth.Client{
		TenantID: cfg.TenantID,
		ClientID: config.BootstrapClientID,
		Scopes:   registerScopes,
		CacheKey: "register-caldova",
		Path:     path,
	}).Token(ctx)
	if err != nil {
		return err
	}
	if _, err := requireCaldovaToken(token); err != nil {
		return err
	}
	blueprintID, err := applicationObjectID(ctx, token, cfg.ClientID)
	if err != nil || blueprintID == "" {
		blueprintID = cfg.ClientID
	}
	access, missing, err := requiredAccess(ctx, token)
	if err != nil {
		return err
	}
	for _, name := range missing {
		if name == "ContentActivity.Write" || name == "ProtectionScopes.Compute.User" {
			return fmt.Errorf("Graph has not published %s in Caldova, so the blueprint grant cannot include it", name)
		}
	}
	if err := configureBlueprint(ctx, token, blueprintID, cfg, access); err != nil {
		return err
	}
	principalID, err := graphGetID(ctx, token, "https://graph.microsoft.com/v1.0/servicePrincipals(appId='"+cfg.ClientID+"')?$select=id")
	if err != nil {
		return fmt.Errorf("blueprint principal was not found: %w", err)
	}
	if err := grantAdminConsent(ctx, token, principalID); err != nil {
		return fmt.Errorf("blueprint consent was not updated: %w", err)
	}
	if err := publishAgentCard(ctx, token, cfg.AgentID, cfg); err != nil {
		return err
	}
	return verifySync(ctx, token, cfg)
}

func publishAgentCard(ctx context.Context, token, registrationID string, cfg config.Config) error {
	if registrationID == "" {
		registrationID = cfg.AgentID
	}
	endpoint := "https://graph.microsoft.com/beta/copilot/agentRegistrations/" + url.PathEscape(registrationID)
	_, err := graphJSON(ctx, token, http.MethodPatch, endpoint, map[string]any{
		"agentCard":                  agentCard(cfg),
		"sourceLastModifiedDateTime": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("agent card was not published: %w", err)
	}
	return nil
}

func agentCard(cfg config.Config) map[string]any {
	return map[string]any{
		"protocolVersion":    "0.3.0",
		"name":               cfg.AgentName,
		"displayName":        cfg.AgentName,
		"description":        cfg.Description,
		"originatingStore":   "Google Vertex",
		"provider": map[string]any{
			"organization": "Google Vertex",
			"url":          "https://cloud.google.com/vertex-ai",
		},
		"version":            "2.0.0",
		"documentationUrl":   "https://github.com/mward-msft1/Version_2_GCP",
		"defaultInputModes":  []string{"text/plain", "application/json"},
		"defaultOutputModes": []string{"text/plain", "application/json"},
		"capabilities": map[string]any{
			"streaming":         true,
			"pushNotifications": false,
		},
		"skills": []map[string]any{
			skill("run_dlp_test", "Run DLP test", "Runs the DocSite DLP scenario for CharlotteW or BrookeG. Uses WorkIQ for mail and Teams, Graph when WorkIQ denies a path, and Purview before every outbound action.", []string{"dlp", "purview", "workiq", "graph"}, []string{"Run the DLP test for CharlotteW using USSocialSecurityNumbers--(x10)Pos.docx"}),
			skill("workiq_send_mail", "Send mail through WorkIQ", "Sends the test document to an approved internal recipient and to mward042@gmail.com through the WorkIQ sendMail action. Falls back to Microsoft Graph only when WorkIQ cannot send.", []string{"workiq", "mail"}, []string{"Send the DocSite file to the approved internal and external recipients"}),
			skill("workiq_post_teams_chat", "Post the file in a Teams chat", "Posts the test document in the approved Teams chat through WorkIQ. Falls back to Microsoft Graph chat messages when WorkIQ is unavailable.", []string{"workiq", "teams", "chat"}, []string{"Post the DocSite file in the Teams chat"}),
			skill("workiq_post_teams_channel", "Post the file in a Teams channel", "Posts the test document in the approved Teams channel through WorkIQ. Falls back to Microsoft Graph channel messages when WorkIQ is unavailable.", []string{"workiq", "teams", "channel"}, []string{"Post the DocSite file in the Teams channel"}),
			skill("list_docsite_documents", "List DocSite documents", "Lists documents in the DocSite library. WorkIQ site listing is policy-denied, so this function uses the Microsoft Graph fallback.", []string{"graph", "sharepoint"}, []string{"List the sensitive test documents in DocSite"}),
			skill("download_docsite_file", "Download a DocSite file", "Downloads the selected DocSite file. Uses WorkIQ when the item path is allowed and Microsoft Graph when it is not.", []string{"graph", "workiq", "sharepoint"}, []string{"Download USSocialSecurityNumbers--(x10)Pos.docx"}),
			skill("purview_record_prompt", "Record the user prompt", "Sends the user prompt to Purview as an uploadText interaction and records the matching content activity.", []string{"purview", "prompt"}, []string{"Record this prompt in Purview"}),
			skill("purview_record_response", "Record the agent response", "Sends the agent response to Purview as a downloadText interaction and records the matching content activity.", []string{"purview", "response"}, []string{"Record this response in Purview"}),
			skill("purview_evaluate_content", "Evaluate sensitive content", "Submits message text and file bytes to Purview processContent before mail or Teams actions. A block skips the outbound action.", []string{"purview", "dlp"}, []string{"Check the DocSite file against Purview before sending it"}),
		},
	}
}

func skill(id, name, description string, tags, examples []string) map[string]any {
	return map[string]any{
		"id":          id,
		"name":        name,
		"displayName": name,
		"description": description,
		"tags":        tags,
		"examples":    examples,
		"inputModes":  []string{"text/plain", "application/json"},
		"outputModes": []string{"text/plain", "application/json"},
	}
}

func verifySync(ctx context.Context, token string, cfg config.Config) error {
	registration, err := graphJSON(ctx, token, http.MethodGet, "https://graph.microsoft.com/beta/copilot/agentRegistrations/"+url.PathEscape(cfg.AgentID), nil)
	if err != nil {
		return err
	}
	card, _ := registration["agentCard"].(map[string]any)
	if cardStore := str(card, "originatingStore"); cardStore != "Google Vertex" {
		return fmt.Errorf("agent card store is %q, expected Google Vertex", cardStore)
	}
	skills, _ := card["skills"].([]any)
	if len(skills) == 0 {
		return fmt.Errorf("agent card was saved without skills")
	}
	fmt.Printf("Agent card skills: %d\n", len(skills))
	for _, item := range skills {
		skill, _ := item.(map[string]any)
		fmt.Printf("  - %s\n", str(skill, "id"))
	}
	principalID, err := graphGetID(ctx, token, "https://graph.microsoft.com/v1.0/servicePrincipals(appId='"+cfg.ClientID+"')?$select=id")
	if err != nil {
		return err
	}
	var grants struct {
		Value []struct {
			ClientID string `json:"clientId"`
			Scope    string `json:"scope"`
		} `json:"value"`
	}
	if err := graphInto(ctx, token, http.MethodGet, "https://graph.microsoft.com/v1.0/oauth2PermissionGrants?$filter="+url.QueryEscape("clientId eq '"+principalID+"'"), nil, &grants); err != nil {
		return err
	}
	if len(grants.Value) == 0 {
		return fmt.Errorf("blueprint principal has no consent grant")
	}
	scope := grants.Value[0].Scope
	for _, required := range []string{"Content.Process.User", "ContentActivity.Write", "ProtectionScopes.Compute.User"} {
		if !strings.Contains(scope, required) {
			return fmt.Errorf("blueprint consent is still missing %s", required)
		}
	}
	fmt.Println("Blueprint consent includes Content.Process.User, ContentActivity.Write, and ProtectionScopes.Compute.User.")
	return nil
}

func requireCaldovaTenant(tenantID string) error {
	if strings.EqualFold(tenantID, "72f988bf-86f1-41af-91ab-2d7cd011db47") {
		return fmt.Errorf("refusing corporate tenant 72f988bf-86f1-41af-91ab-2d7cd011db47; registration must use Caldova %s", config.TenantID)
	}
	if !strings.EqualFold(tenantID, config.TenantID) {
		return fmt.Errorf("refusing tenant %s; only Caldova %s is allowed", tenantID, config.TenantID)
	}
	return nil
}

func requireCaldovaToken(token string) (string, error) {
	claims, err := jwtClaims(token)
	if err != nil {
		return "", err
	}
	tid, _ := claims["tid"].(string)
	if err := requireCaldovaTenant(tid); err != nil {
		return "", fmt.Errorf("token rejected: %w", err)
	}
	user := firstString(claims, "upn", "preferred_username", "unique_name")
	if user == "" {
		return "", fmt.Errorf("Caldova token has no user principal name")
	}
	if !strings.HasSuffix(strings.ToLower(user), "@caldova56317036.onmicrosoft.com") && !strings.Contains(strings.ToLower(user), "caldova56317036") {
		return "", fmt.Errorf("signed-in user %s is not a Caldova account", user)
	}
	return user, nil
}

func jwtClaims(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("access token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil, fmt.Errorf("decode token claims: %w", err)
		}
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, err
	}
	return claims, nil
}

func firstString(claims map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := claims[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func existingBlueprintAppID(ctx context.Context, token, name string) (string, error) {
	var body struct {
		Value []struct {
			AppID string `json:"appId"`
			Type  string `json:"@odata.type"`
		} `json:"value"`
	}
	endpoint := "https://graph.microsoft.com/v1.0/applications?$filter=" + url.QueryEscape("displayName eq '"+strings.ReplaceAll(name, "'", "''")+"'") + "&$select=appId"
	if err := graphInto(ctx, token, http.MethodGet, endpoint, nil, &body); err != nil {
		return "", err
	}
	var ids []string
	for _, app := range body.Value {
		if app.AppID == "" {
			continue
		}
		if app.Type == "" || strings.Contains(strings.ToLower(app.Type), "agentidentityblueprint") {
			ids = append(ids, app.AppID)
		}
	}
	if len(ids) > 1 {
		return "", fmt.Errorf("multiple Agent 365 blueprints are named %s; keep the original and remove the duplicate before registering again", name)
	}
	if len(ids) == 1 {
		return ids[0], nil
	}
	return "", nil
}

func applicationObjectID(ctx context.Context, token, appID string) (string, error) {
	var body struct {
		Value []struct {
			ID string `json:"id"`
		} `json:"value"`
	}
	endpoint := "https://graph.microsoft.com/v1.0/applications?$filter=" + url.QueryEscape("appId eq '"+appID+"'") + "&$select=id"
	if err := graphInto(ctx, token, http.MethodGet, endpoint, nil, &body); err != nil {
		return "", err
	}
	if len(body.Value) == 0 {
		return "", fmt.Errorf("blueprint %s was not found in Caldova", appID)
	}
	return body.Value[0].ID, nil
}

func graphGetID(ctx context.Context, token, endpoint string) (string, error) {
	var body map[string]any
	if err := graphInto(ctx, token, http.MethodGet, endpoint, nil, &body); err != nil {
		return "", err
	}
	return str(body, "id"), nil
}

func graphJSON(ctx context.Context, token, method, endpoint string, payload any) (map[string]any, error) {
	var out map[string]any
	err := graphInto(ctx, token, method, endpoint, payload, &out)
	return out, err
}

func graphInto(ctx context.Context, token, method, endpoint string, payload any, out any) error {
	var reader io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("OData-Version", "4.0")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s", method, endpoint, truncate(body))
	}
	if out == nil || len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	return json.Unmarshal(body, out)
}

func str(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func truncate(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 600 {
		return s[:600]
	}
	return s
}
