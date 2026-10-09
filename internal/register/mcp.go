package register

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/mward-msft1/Version_2_GCP/internal/catalog"
	"github.com/mward-msft1/Version_2_GCP/internal/config"
)

// GrantCatalog adds the Work IQ catalog servers to the blueprint and runtime
// client, then grants tenant-wide delegated consent. It merges with existing
// Graph access instead of replacing it.
func GrantCatalog(ctx context.Context, token string, cfg config.Config) error {
	if err := requireCaldovaTenant(cfg.TenantID); err != nil {
		return err
	}
	servers := catalog.Load("ToolingManifest.json")
	access, err := catalogAccess(ctx, token, servers)
	if err != nil {
		return err
	}
	blueprintID, err := applicationObjectID(ctx, token, cfg.ClientID)
	if err != nil || blueprintID == "" {
		blueprintID = cfg.ClientID
	}
	if err := mergeRequiredAccess(ctx, token, "https://graph.microsoft.com/v1.0/applications/microsoft.graph.agentIdentityBlueprint/"+url.PathEscape(cfg.ClientID), access); err != nil {
		return fmt.Errorf("blueprint Work IQ access was not updated: %w", err)
	}
	for _, item := range access {
		resourceID, _ := item["resourceAppId"].(string)
		if err := inheritScopes(ctx, token, blueprintID, resourceID); err != nil {
			return err
		}
	}
	principalID, err := graphGetID(ctx, token, "https://graph.microsoft.com/v1.0/servicePrincipals(appId='"+cfg.ClientID+"')?$select=id")
	if err != nil {
		return fmt.Errorf("blueprint principal was not found: %w", err)
	}
	clients := []string{principalID}
	if cfg.RuntimeClientID != "" {
		runtimeObject, objectErr := applicationObjectID(ctx, token, cfg.RuntimeClientID)
		if objectErr == nil && runtimeObject != "" {
			if err := mergeRequiredAccess(ctx, token, "https://graph.microsoft.com/v1.0/applications/"+url.PathEscape(runtimeObject), access); err != nil {
				return fmt.Errorf("runtime Work IQ access was not updated: %w", err)
			}
		}
		runtimeSP, spErr := graphGetID(ctx, token, "https://graph.microsoft.com/v1.0/servicePrincipals(appId='"+cfg.RuntimeClientID+"')?$select=id")
		if spErr != nil {
			return fmt.Errorf("runtime service principal was not found: %w", spErr)
		}
		clients = append(clients, runtimeSP)
	}
	for _, item := range access {
		resourceAppID, _ := item["resourceAppId"].(string)
		resourceSP, err := graphGetID(ctx, token, "https://graph.microsoft.com/v1.0/servicePrincipals(appId='"+resourceAppID+"')?$select=id")
		if err != nil {
			return fmt.Errorf("Work IQ resource %s was not found: %w", resourceAppID, err)
		}
		for _, clientID := range clients {
			if err := grantResource(ctx, token, clientID, resourceSP, "Tools.ListInvoke.All"); err != nil {
				return err
			}
		}
	}
	return nil
}

func catalogAccess(ctx context.Context, token string, servers []catalog.Server) ([]map[string]any, error) {
	var access []map[string]any
	for _, server := range servers {
		if server.Audience == "" {
			return nil, fmt.Errorf("Work IQ %s has no audience", server.Name)
		}
		var sp struct {
			AppID  string `json:"appId"`
			Scopes []struct {
				ID    string `json:"id"`
				Value string `json:"value"`
			} `json:"oauth2PermissionScopes"`
		}
		endpoint := "https://graph.microsoft.com/v1.0/servicePrincipals(appId='" + server.Audience + "')?$select=appId,oauth2PermissionScopes"
		if err := graphInto(ctx, token, http.MethodGet, endpoint, nil, &sp); err != nil {
			return nil, fmt.Errorf("read %s scopes: %w", server.Name, err)
		}
		want := server.Scope
		if want == "" {
			want = "Tools.ListInvoke.All"
		}
		var scopes []any
		for _, scope := range sp.Scopes {
			if strings.EqualFold(scope.Value, want) {
				scopes = append(scopes, map[string]any{"id": scope.ID, "type": "Scope"})
				break
			}
		}
		if len(scopes) == 0 {
			return nil, fmt.Errorf("%s does not publish %s", server.Name, want)
		}
		access = append(access, map[string]any{
			"resourceAppId":  server.Audience,
			"resourceAccess": scopes,
		})
	}
	return access, nil
}

func mergeRequiredAccess(ctx context.Context, token, endpoint string, additions []map[string]any) error {
	var current struct {
		Required []map[string]any `json:"requiredResourceAccess"`
	}
	if err := graphInto(ctx, token, http.MethodGet, endpoint+"?$select=requiredResourceAccess", nil, &current); err != nil {
		return err
	}
	merged := current.Required
	for _, add := range additions {
		resourceID, _ := add["resourceAppId"].(string)
		replaced := false
		for i, item := range merged {
			if strings.EqualFold(fmt.Sprint(item["resourceAppId"]), resourceID) {
				merged[i] = add
				replaced = true
				break
			}
		}
		if !replaced {
			merged = append(merged, add)
		}
	}
	_, err := graphJSON(ctx, token, http.MethodPatch, endpoint, map[string]any{"requiredResourceAccess": merged})
	return err
}

func inheritScopes(ctx context.Context, token, blueprintID, resourceAppID string) error {
	_, err := graphJSON(ctx, token, http.MethodPost, "https://graph.microsoft.com/v1.0/applications/microsoft.graph.agentIdentityBlueprint/"+url.PathEscape(blueprintID)+"/inheritablePermissions", map[string]any{
		"resourceAppId": resourceAppID,
		"inheritableScopes": map[string]any{
			"@odata.type": "#microsoft.graph.enumeratedScopes",
			"kind":        "enumerated",
			"scopes":      []string{"Tools.ListInvoke.All"},
		},
		"inheritableRoles": map[string]any{
			"@odata.type": "#microsoft.graph.noRoles",
			"kind":        "none",
		},
	})
	if err != nil && !strings.Contains(err.Error(), "409") && !strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return fmt.Errorf("inheritable %s scopes were not set: %w", resourceAppID, err)
	}
	return nil
}

func grantResource(ctx context.Context, token, clientID, resourceID, scope string) error {
	_, err := graphJSON(ctx, token, http.MethodPost, "https://graph.microsoft.com/v1.0/oauth2PermissionGrants", map[string]any{
		"clientId":    clientID,
		"consentType": "AllPrincipals",
		"resourceId":  resourceID,
		"scope":       scope,
	})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return err
	}
	var grants struct {
		Value []struct {
			ID         string `json:"id"`
			ResourceID string `json:"resourceId"`
		} `json:"value"`
	}
	list := "https://graph.microsoft.com/v1.0/oauth2PermissionGrants?$filter=" + url.QueryEscape("clientId eq '"+clientID+"'")
	if listErr := graphInto(ctx, token, http.MethodGet, list, nil, &grants); listErr != nil {
		return err
	}
	for _, grant := range grants.Value {
		if !strings.EqualFold(grant.ResourceID, resourceID) {
			continue
		}
		_, patchErr := graphJSON(ctx, token, http.MethodPatch, "https://graph.microsoft.com/v1.0/oauth2PermissionGrants/"+url.PathEscape(grant.ID), map[string]any{
			"scope": scope,
		})
		return patchErr
	}
	return err
}
