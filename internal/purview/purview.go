package purview

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Decision struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason"`
	Status  int    `json:"status"`
}

type Client struct {
	Token     string
	AppID     string
	AppName   string
	UserEmail string
	HTTP      *http.Client
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// Evaluate sends an interaction to Microsoft Purview processContent so DSPM
// and DLP can inspect it. Any policy action blocks the activity. Evaluation
// errors also block, so a send cannot proceed without a monitoring decision.
func (c *Client) Evaluate(ctx context.Context, activity, text string) (Decision, error) {
	if strings.TrimSpace(c.Token) == "" {
		return Decision{Allowed: false, Reason: "no Microsoft Graph token; run login before the DLP test"}, nil
	}
	user := c.UserEmail
	if user == "" {
		user = "me"
	}
	now := time.Now().UTC().Format(time.RFC3339)
	payload := map[string]any{
		"contentToProcess": map[string]any{
			"contentEntries": []any{
				map[string]any{
					"@odata.type":      "microsoft.graph.processConversationMetadata",
					"identifier":       uuid.NewString(),
					"name":             c.AppName,
					"correlationId":    uuid.NewString(),
					"sequenceNumber":   0,
					"isTruncated":      false,
					"createdDateTime":  now,
					"modifiedDateTime": now,
					"contentCategory":  "ai",
					"content": map[string]any{
						"@odata.type": "microsoft.graph.textContent",
						"data":        truncate(text, 12000),
					},
				},
			},
			"activityMetadata": map[string]any{"activity": activity},
			"deviceMetadata": map[string]any{
				"deviceType": "Unmanaged",
				"operatingSystemSpecifications": map[string]any{
					"operatingSystemPlatform": "Windows",
					"operatingSystemVersion":  "10.0",
				},
			},
			"protectedAppMetadata": map[string]any{
				"name":    c.AppName,
				"version": "2.0",
				"applicationLocation": map[string]any{
					"@odata.type": "microsoft.graph.policyLocationApplication",
					"value":       c.AppID,
				},
			},
			"integratedAppMetadata": map[string]any{
				"name":    c.AppName,
				"version": "2.0",
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Decision{}, err
	}
	endpoint := fmt.Sprintf("https://graph.microsoft.com/beta/users/%s/dataSecurityAndGovernance/processContent", url.PathEscape(user))
	if user == "me" {
		endpoint = "https://graph.microsoft.com/beta/me/dataSecurityAndGovernance/processContent"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Decision{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Client-Request-Id", uuid.NewString())
	resp, err := c.client().Do(req)
	if err != nil {
		return Decision{Allowed: false, Reason: "Purview processContent request failed: " + err.Error()}, nil
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return Decide(resp.StatusCode, respBody), nil
}

func Decide(status int, body []byte) Decision {
	switch status {
	case http.StatusNoContent:
		return Decision{Allowed: true, Status: status, Reason: "Purview returned no policy action"}
	case http.StatusAccepted:
		return Decision{Allowed: false, Status: status, Reason: "Purview accepted the content asynchronously and did not return a block/allow decision"}
	}
	if status < 200 || status >= 300 {
		return Decision{Allowed: false, Status: status, Reason: "Purview processContent failed: " + truncate(string(body), 500)}
	}
	var parsed struct {
		PolicyActions []json.RawMessage `json:"policyActions"`
		Errors        []json.RawMessage `json:"processingErrors"`
	}
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &parsed); err != nil {
			return Decision{Allowed: false, Status: status, Reason: "Purview response could not be parsed"}
		}
	}
	if len(parsed.Errors) > 0 {
		return Decision{Allowed: false, Status: status, Reason: "Purview processingErrors: " + truncate(string(body), 500)}
	}
	if len(parsed.PolicyActions) > 0 || strings.Contains(strings.ToLower(string(body)), "restrictaccess") {
		return Decision{Allowed: false, Status: status, Reason: "Purview DLP policy action blocked the activity"}
	}
	return Decision{Allowed: true, Status: status, Reason: "Purview returned no restrictive policy action"}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
