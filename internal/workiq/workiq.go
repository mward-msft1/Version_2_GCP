package workiq

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Client calls a WorkIQ MCP endpoint when one is configured. Microsoft Graph
// remains the execution path when the endpoint is absent or the tool is missing.
type Client struct {
	URL   string
	Token string
	HTTP  *http.Client
}

func (c *Client) Enabled() bool {
	return c != nil && strings.TrimSpace(c.URL) != ""
}

func (c *Client) Call(ctx context.Context, name string, args map[string]any) (json.RawMessage, error) {
	if !c.Enabled() {
		return nil, fmt.Errorf("WorkIQ MCP is not configured")
	}
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      name,
			"arguments": args,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("WorkIQ MCP %s failed: %s", name, truncate(respBody))
	}
	var parsed struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return respBody, nil
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("WorkIQ MCP %s: %s", name, parsed.Error.Message)
	}
	return parsed.Result, nil
}

func truncate(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 400 {
		return s[:400]
	}
	return s
}
