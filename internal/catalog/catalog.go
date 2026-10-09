package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mward-msft1/Version_2_GCP/internal/auth"
	"github.com/mward-msft1/Version_2_GCP/internal/config"
)

const protocolVersion = "2025-03-26"

// Servers are the Agent 365 Work IQ catalog entries the agent is allowed to call.
// User is mcp_MeServer. The CLI catalog did not publish it, so its audience is the
// Caldova service principal created for app 147dc821-b413-44c0-8009-1a3098378012.
var Servers = []Server{
	{Name: "mcp_OneDriveRemoteServer", Alias: "onedrive", URL: "https://agent365.svc.cloud.microsoft/agents/servers/mcp_OneDriveRemoteServer", Scope: "Tools.ListInvoke.All", Audience: "b0b2a2bb-6361-4549-a00c-a018417eb8e2"},
	{Name: "mcp_CalendarTools", Alias: "calendar", URL: "https://agent365.svc.cloud.microsoft/agents/servers/mcp_CalendarTools", Scope: "Tools.ListInvoke.All", Audience: "910333d2-47e9-43ca-981f-6df2f4531ef4"},
	{Name: "mcp_WordServer", Alias: "word", URL: "https://agent365.svc.cloud.microsoft/agents/servers/mcp_WordServer", Scope: "Tools.ListInvoke.All", Audience: "c2d0c2b6-8013-4346-9f8b-b81d3b754a29"},
	{Name: "mcp_M365Copilot", Alias: "copilot", URL: "https://agent365.svc.cloud.microsoft/agents/servers/mcp_M365Copilot", Scope: "Tools.ListInvoke.All", Audience: "ab7c82de-7946-4454-ac28-70249d17c95e"},
	{Name: "mcp_MeServer", Alias: "user", URL: "https://agent365.svc.cloud.microsoft/agents/servers/mcp_MeServer", Scope: "Tools.ListInvoke.All", Audience: "147dc821-b413-44c0-8009-1a3098378012"},
}

type Server struct {
	Name     string `json:"mcpServerName"`
	Alias    string `json:"alias,omitempty"`
	URL      string `json:"url"`
	Scope    string `json:"scope"`
	Audience string `json:"audience"`
}

type Request struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

type Result struct {
	Server  string `json:"server"`
	Tool    string `json:"tool"`
	Output  string `json:"output"`
	Blocked bool   `json:"blocked,omitempty"`
}

type Inspector func(ctx context.Context, text string) (allowed bool, reason string, err error)

type Client struct {
	Servers   []Server
	Tokens    map[string]string
	HTTP      *http.Client
	Inspector Inspector

	mu       sync.Mutex
	sessions map[string]string
	nextID   int
}

func Load(path string) []Server {
	out := append([]Server(nil), Servers...)
	raw, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var file struct {
		MCPServers []Server `json:"mcpServers"`
	}
	if json.Unmarshal(raw, &file) != nil {
		return out
	}
	byName := map[string]Server{}
	for _, server := range file.MCPServers {
		byName[server.Name] = server
	}
	for i, server := range out {
		found, ok := byName[server.Name]
		if !ok {
			continue
		}
		if found.URL != "" {
			out[i].URL = found.URL
		}
		if found.Scope != "" {
			out[i].Scope = found.Scope
		}
		if found.Audience != "" {
			out[i].Audience = found.Audience
		}
	}
	return out
}

func ByAlias(servers []Server, alias string) (Server, error) {
	alias = strings.ToLower(strings.TrimSpace(alias))
	for _, server := range servers {
		if strings.EqualFold(server.Alias, alias) || strings.EqualFold(server.Name, alias) {
			return server, nil
		}
	}
	return Server{}, fmt.Errorf("unknown Work IQ catalog server %q", alias)
}

func Auth(cfg config.Config, user string, server Server) (*auth.Client, error) {
	path, err := cfg.TokenPath()
	if err != nil {
		return nil, err
	}
	user = strings.ToLower(strings.TrimSpace(user))
	if user == "" || cfg.RuntimeClientID == "" || server.Audience == "" {
		return nil, fmt.Errorf("Work IQ %s sign-in needs the runtime client, acting user, and audience", server.Alias)
	}
	scope := server.Scope
	if scope == "" {
		scope = "Tools.ListInvoke.All"
	}
	return &auth.Client{
		TenantID: cfg.TenantID,
		ClientID: cfg.RuntimeClientID,
		Scopes:   []string{server.Audience + "/" + scope, "offline_access"},
		CacheKey: "mcp:" + server.Audience + ":" + user,
		Path:     path,
	}, nil
}

func CachedTokens(ctx context.Context, cfg config.Config, user string, servers []Server) (map[string]string, error) {
	tokens := map[string]string{}
	var missing []string
	for _, server := range servers {
		client, err := Auth(cfg, user, server)
		if err != nil {
			return nil, err
		}
		token, err := client.Refresh(ctx)
		if err != nil {
			missing = append(missing, server.Alias)
			continue
		}
		tokens[server.Audience] = token
	}
	if len(missing) > 0 {
		return tokens, fmt.Errorf("Work IQ catalog sign-in missing for %s; run login as %s", strings.Join(missing, ", "), user)
	}
	return tokens, nil
}

func Login(ctx context.Context, cfg config.Config, user string, servers []Server) error {
	for _, server := range servers {
		client, err := Auth(cfg, user, server)
		if err != nil {
			return err
		}
		fmt.Printf("Work IQ %s (%s) needs a delegated %s token.\n", server.Alias, server.Name, server.Scope)
		if _, err := client.Token(ctx); err != nil {
			return fmt.Errorf("Work IQ %s sign-in failed: %w", server.Alias, err)
		}
	}
	return nil
}

func (c *Client) Invoke(ctx context.Context, alias string, req Request) (Result, error) {
	servers := c.Servers
	if len(servers) == 0 {
		servers = Servers
	}
	server, err := ByAlias(servers, alias)
	if err != nil {
		return Result{}, err
	}
	result := Result{Server: server.Name, Tool: req.Tool}
	if c.Inspector != nil {
		payload, _ := json.Marshal(req)
		allowed, reason, inspectErr := c.Inspector(ctx, server.Alias+" "+string(payload))
		if inspectErr != nil || !allowed {
			if reason == "" && inspectErr != nil {
				reason = inspectErr.Error()
			}
			result.Blocked = true
			result.Output = "Purview blocked this Work IQ tool call: " + reason
			return result, nil
		}
	}
	token := ""
	if c.Tokens != nil {
		token = c.Tokens[server.Audience]
	}
	if token == "" {
		return result, fmt.Errorf("Work IQ %s sign-in missing; run login as CharlotteW or BrookeG", server.Alias)
	}
	name := strings.TrimSpace(req.Tool)
	method := "tools/call"
	params := map[string]any{"name": name, "arguments": req.Arguments}
	if name == "" || strings.EqualFold(name, "list") {
		method = "tools/list"
		params = map[string]any{}
		result.Tool = "list"
	}
	raw, err := c.rpc(ctx, server, token, method, params)
	if err != nil {
		return result, err
	}
	result.Output = string(raw)
	return result, nil
}

func (c *Client) rpc(ctx context.Context, server Server, token, method string, params any) (json.RawMessage, error) {
	if err := c.ensureSession(ctx, server, token); err != nil {
		return nil, err
	}
	raw, err := c.post(ctx, server, token, method, params)
	if err != nil {
		c.clearSession(server.URL)
		return nil, fmt.Errorf("Work IQ %s %s: %w", server.Alias, method, err)
	}
	return raw, nil
}

func (c *Client) ensureSession(ctx context.Context, server Server, token string) error {
	c.mu.Lock()
	if c.sessions == nil {
		c.sessions = map[string]string{}
	}
	ready := c.sessions[server.URL] != ""
	c.mu.Unlock()
	if ready {
		return nil
	}
	if _, err := c.post(ctx, server, token, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "caldova-gcp-agent-v2", "version": "2"},
	}); err != nil {
		return fmt.Errorf("Work IQ %s initialize: %w", server.Alias, err)
	}
	note, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	req, err := c.newRequest(ctx, server.URL, token, note)
	if err != nil {
		return err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("Work IQ %s initialized notification failed: %s", server.Alias, resp.Status)
	}
	return nil
}

func (c *Client) post(ctx context.Context, server Server, token, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	req, err := c.newRequest(ctx, server.URL, token, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if sessionID := resp.Header.Get("Mcp-Session-Id"); sessionID != "" {
		c.mu.Lock()
		if c.sessions == nil {
			c.sessions = map[string]string{}
		}
		c.sessions[server.URL] = sessionID
		c.mu.Unlock()
	}
	if resp.StatusCode >= 300 {
		c.clearSession(server.URL)
		return nil, fmt.Errorf("%s", truncate(respBody))
	}
	var parsed struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(firstJSON(respBody), &parsed); err != nil {
		return nil, fmt.Errorf("%s", truncate(respBody))
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("%s", parsed.Error.Message)
	}
	if len(parsed.Result) == 0 {
		return json.RawMessage(`{}`), nil
	}
	return parsed.Result, nil
}

func (c *Client) newRequest(ctx context.Context, endpoint, token string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", protocolVersion)
	c.mu.Lock()
	sessionID := ""
	if c.sessions != nil {
		sessionID = c.sessions[endpoint]
	}
	c.mu.Unlock()
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req, nil
}

func (c *Client) clearSession(endpoint string) {
	c.mu.Lock()
	delete(c.sessions, endpoint)
	c.mu.Unlock()
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 2 * time.Minute}
}

func firstJSON(body []byte) []byte {
	body = bytes.TrimSpace(body)
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		line = bytes.TrimPrefix(line, []byte("data:"))
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("{")) {
			return line
		}
	}
	return body
}

func truncate(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 400 {
		return text[:400]
	}
	if text == "" {
		return "empty MCP response"
	}
	return text
}
