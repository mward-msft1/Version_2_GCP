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
	Token           string
	AppID           string
	AppName         string
	UserEmail       string
	BlueprintID     string
	AgentIdentityID string
	HTTP            *http.Client

	scopeETag   string
	correlation string
	sequence    int
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// Evaluate sends one interaction to Microsoft Purview. processContent lets the
// published DLP policy return an action. This agent does not contain policy rules.
func (c *Client) Evaluate(ctx context.Context, activity, text string) (Decision, error) {
	return c.Inspect(ctx, activity, text, uuid.NewString(), 1)
}

// EvaluatePrompt records the user prompt and starts the response pair.
func (c *Client) EvaluatePrompt(ctx context.Context, text string) (Decision, error) {
	c.correlation = uuid.NewString()
	c.sequence = 1
	return c.Inspect(ctx, "uploadText", text, c.correlation, c.sequence)
}

// EvaluateResponse records the agent response against the prompt correlation.
func (c *Client) EvaluateResponse(ctx context.Context, text string) (Decision, error) {
	if c.correlation == "" {
		c.correlation = uuid.NewString()
		c.sequence = 1
	}
	c.sequence++
	return c.Inspect(ctx, "downloadText", text, c.correlation, c.sequence)
}

// Inspect evaluates one prompt or response and records it for DSPM capture.
// uploadText is the user prompt. downloadText is the agent response. The same
// correlation ID and increasing sequence number pair the two in Activity explorer.
func (c *Client) Inspect(ctx context.Context, activity, text, correlationID string, sequence int) (Decision, error) {
	if strings.TrimSpace(c.Token) == "" {
		return Decision{Allowed: false, Reason: "no Microsoft Graph token; run login before the DLP test"}, nil
	}
	decision, err := c.processContent(ctx, activity, text, correlationID, sequence)
	if err != nil {
		return decision, err
	}
	if note := c.recordActivity(ctx, activity, correlationID, sequence); note != "" {
		decision.Reason = strings.TrimSpace(decision.Reason + "; " + note)
	}
	return decision, nil
}

func (c *Client) processContent(ctx context.Context, activity, text, correlationID string, sequence int) (Decision, error) {
	payload := c.payload(activity, text, correlationID, sequence, true)
	body, err := json.Marshal(payload)
	if err != nil {
		return Decision{}, err
	}
	c.refreshScopes(ctx)
	endpoint := c.endpoint("processContent")
	respBody, status, err := c.post(ctx, endpoint, body)
	if err != nil {
		return Decision{Allowed: false, Reason: "Purview processContent request failed: " + err.Error()}, nil
	}
	return Decide(status, respBody), nil
}

func (c *Client) recordActivity(ctx context.Context, activity, correlationID string, sequence int) string {
	payload := c.payload(activity, "", correlationID, sequence, false)
	body, err := json.Marshal(payload)
	if err != nil {
		return "contentActivities payload failed: " + err.Error()
	}
	respBody, status, err := c.post(ctx, c.endpoint("activities/contentActivities"), body)
	if err != nil {
		return "contentActivities request failed: " + err.Error()
	}
	if status < 200 || status >= 300 {
		return "contentActivities was not recorded: " + truncate(string(respBody), 300)
	}
	return "contentActivities recorded " + activity
}

func (c *Client) payload(activity, text, correlationID string, sequence int, includeContent bool) map[string]any {
	now := time.Now().UTC().Format(time.RFC3339)
	if correlationID == "" {
		correlationID = uuid.NewString()
	}
	if sequence < 1 {
		sequence = 1
	}
	entry := map[string]any{
		"@odata.type":      "microsoft.graph.processConversationMetadata",
		"identifier":       uuid.NewString(),
		"name":             c.AppName,
		"correlationId":    correlationID,
		"sequenceNumber":   sequence,
		"isTruncated":      len(text) > 12000,
		"createdDateTime":  now,
		"modifiedDateTime": now,
		"agents":           c.agents(),
	}
	if includeContent {
		entry["contentCategory"] = "ai"
		entry["content"] = map[string]any{
			"@odata.type": "microsoft.graph.textContent",
			"data":        truncate(text, 12000),
		}
	}
	return map[string]any{
		"contentToProcess": map[string]any{
			"contentEntries":   []any{entry},
			"activityMetadata": map[string]any{"activity": activity},
			"deviceMetadata": map[string]any{
				"operatingSystemSpecifications": map[string]any{
					"operatingSystemPlatform": "Windows 11",
					"operatingSystemVersion":  "10.0.26100.0",
				},
				"deviceType": "Unmanaged",
				"ipAddress":  "127.0.0.1",
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
}

func (c *Client) agents() []any {
	agent := map[string]any{
		"name":    c.AppName,
		"version": "2.0",
	}
	if c.AgentIdentityID != "" {
		agent["identifier"] = c.AgentIdentityID
	} else {
		agent["identifier"] = c.AppID
	}
	if c.BlueprintID != "" {
		agent["blueprintId"] = c.BlueprintID
	}
	return []any{agent}
}

func (c *Client) endpoint(action string) string {
	user := c.UserEmail
	if user == "" || user == "me" {
		return "https://graph.microsoft.com/beta/me/dataSecurityAndGovernance/" + action
	}
	return fmt.Sprintf("https://graph.microsoft.com/beta/users/%s/dataSecurityAndGovernance/%s", url.PathEscape(user), action)
}

func (c *Client) post(ctx context.Context, endpoint string, body []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Client-Request-Id", uuid.NewString())
	if c.scopeETag != "" && strings.HasSuffix(endpoint, "/processContent") {
		req.Header.Set("If-None-Match", c.scopeETag)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if etag := resp.Header.Get("ETag"); etag != "" {
		c.scopeETag = etag
	}
	respBody, _ := io.ReadAll(resp.Body)
	return respBody, resp.StatusCode, nil
}

// refreshScopes asks Purview which published policies apply to this application.
// An empty result means no policy currently covers the user. It is not a local policy.
func (c *Client) refreshScopes(ctx context.Context) {
	if strings.TrimSpace(c.Token) == "" || c.AppID == "" {
		return
	}
	body, err := json.Marshal(map[string]any{
		"activities": "uploadText,downloadText,uploadFile,downloadFile",
		"locations": []any{map[string]any{
			"@odata.type": "microsoft.graph.policyLocationApplication",
			"value":       c.AppID,
		}},
		"integratedAppMetadata": map[string]any{"name": c.AppName, "version": "2.0"},
	})
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("protectionScopes/compute"), bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Client-Request-Id", uuid.NewString())
	if c.scopeETag != "" {
		req.Header.Set("If-None-Match", c.scopeETag)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if etag := resp.Header.Get("ETag"); etag != "" {
		c.scopeETag = etag
	}
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
