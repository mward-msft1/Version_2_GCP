package workiq

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/mward-msft1/Version_2_GCP/internal/auth"
	"github.com/mward-msft1/Version_2_GCP/internal/config"
	"github.com/mward-msft1/Version_2_GCP/internal/m365"
)

const (
	// Hosted Microsoft 365 WorkIQ MCP server.
	DefaultURL = config.DefaultWorkIQMCPURL
	// Public client published with the WorkIQ MCP server. No secret is stored.
	PublicClientID = "ba081686-5d24-4bc6-a0d6-d034ecffed87"
	// Scope advertised by the WorkIQ protected-resource metadata.
	Scope           = "fdcc1f02-fc51-4226-8753-f668596af7f7/WorkIQAgent.Ask"
	protocolVersion = "2025-03-26"
)

// Client calls the hosted WorkIQ MCP server with the acting user's token.
// Microsoft Graph remains the fallback when WorkIQ is unavailable.
type Client struct {
	URL   string
	Token string
	HTTP  *http.Client

	mu        sync.Mutex
	sessionID string
	nextID    int
}

func Auth(cfg config.Config, user string) (*auth.Client, error) {
	path, err := cfg.TokenPath()
	if err != nil {
		return nil, err
	}
	user = strings.ToLower(strings.TrimSpace(user))
	if user == "" {
		return nil, fmt.Errorf("WorkIQ sign-in needs the acting user")
	}
	return &auth.Client{
		TenantID: cfg.TenantID,
		ClientID: PublicClientID,
		Scopes:   []string{Scope, "offline_access"},
		CacheKey: "workiq:" + user,
		Path:     path,
	}, nil
}

func CachedToken(cfg config.Config, user string) (string, error) {
	client, err := Auth(cfg, user)
	if err != nil {
		return "", err
	}
	return client.CachedToken()
}

func Refresh(ctx context.Context, cfg config.Config, user string) (string, error) {
	client, err := Auth(cfg, user)
	if err != nil {
		return "", err
	}
	return client.Refresh(ctx)
}

func (c *Client) Enabled() bool {
	return c != nil && strings.TrimSpace(c.URL) != "" && !strings.EqualFold(c.URL, "off")
}

func (c *Client) ListDocuments(ctx context.Context, host, sitePath string) (string, []m365.DriveItem, error) {
	// WorkIQ denies /sites/{host}:{path}:/drive/root/children. Resolve the site
	// id, then list /drives/{driveId}/items/{rootId}/children. Do not encode the
	// site id: WorkIQ rejects the encoded commas in a SharePoint site id.
	term := siteSearchTerm(sitePath)
	if term == "" {
		return "", nil, fmt.Errorf("WorkIQ list needs a SharePoint site path")
	}
	raw, err := c.Fetch(ctx, []string{"/sites?search=" + url.QueryEscape(term) + "&$select=id,displayName,name,webUrl&$top=5"})
	if err != nil {
		return "", nil, err
	}
	siteID := siteIDFrom(raw, host, sitePath)
	if siteID == "" {
		return "", nil, fmt.Errorf("WorkIQ found no site for %s%s", host, sitePath)
	}
	driveRaw, err := c.Fetch(ctx, []string{"/sites/" + siteID + "/drive?$expand=root&$select=id,name,webUrl"})
	if err != nil {
		return "", nil, err
	}
	driveID, rootID := driveRootFrom(driveRaw)
	if driveID == "" || rootID == "" {
		return "", nil, fmt.Errorf("WorkIQ did not return a drive root for %s%s", host, sitePath)
	}
	listed, err := c.Fetch(ctx, []string{"/drives/" + driveID + "/items/" + rootID + "/children?$select=id,name,size,webUrl,file,folder,parentReference&$top=50"})
	if err != nil {
		return "", nil, err
	}
	foundDrive, files := filesFrom(listed)
	if foundDrive != "" {
		driveID = foundDrive
	}
	if len(files) == 0 {
		return "", nil, fmt.Errorf("WorkIQ listed no files at %s%s", host, sitePath)
	}
	return driveID, files, nil
}

func (c *Client) Download(ctx context.Context, driveID string, item m365.DriveItem) (m365.FilePayload, error) {
	payload := m365.FilePayload{Item: item, DriveID: driveID, Snippet: item.Name, Link: item.WebURL}
	if driveID == "" || item.ID == "" {
		return payload, fmt.Errorf("WorkIQ download needs a drive id and item id")
	}
	if item.Size == 0 || item.Size <= 3*1024*1024 {
		raw, err := c.FetchBlob(ctx, fmt.Sprintf("/drives/%s/items/%s/content", url.PathEscape(driveID), url.PathEscape(item.ID)))
		if err != nil {
			return payload, err
		}
		encoded := findString(raw, "base64Content")
		if encoded == "" {
			return payload, fmt.Errorf("WorkIQ fetch_blob returned no file bytes")
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return payload, err
		}
		payload.Bytes = decoded
		payload.Snippet = m365.ContentSnippet(item, decoded)
	}
	linkRaw, err := c.DoAction(ctx, fmt.Sprintf("/drives/%s/items/%s/createLink", url.PathEscape(driveID), url.PathEscape(item.ID)), map[string]any{
		"type":  "view",
		"scope": "organization",
	})
	if err == nil {
		if link := findString(linkRaw, "webUrl"); link != "" {
			payload.Link = link
		}
	}
	return payload, nil
}

func (c *Client) SendMail(ctx context.Context, to string, cc []string, subject, bodyText string, file m365.FilePayload) error {
	message := map[string]any{
		"subject": subject,
		"body": map[string]any{
			"contentType": "Text",
			"content":     bodyText,
		},
		"toRecipients": []any{map[string]any{"emailAddress": map[string]any{"address": to}}},
	}
	var copied []any
	for _, address := range cc {
		if address == "" || strings.EqualFold(address, to) {
			continue
		}
		copied = append(copied, map[string]any{"emailAddress": map[string]any{"address": address}})
	}
	if len(copied) > 0 {
		message["ccRecipients"] = copied
	}
	if len(file.Bytes) > 0 {
		message["attachments"] = []any{map[string]any{
			"@odata.type":  "#microsoft.graph.fileAttachment",
			"name":         file.Item.Name,
			"contentBytes": base64.StdEncoding.EncodeToString(file.Bytes),
		}}
	}
	_, err := c.DoAction(ctx, "/me/sendMail", map[string]any{
		"message":         message,
		"saveToSentItems": true,
	})
	return err
}

func (c *Client) PostChat(ctx context.Context, other, text string) (string, error) {
	raw, err := c.Fetch(ctx, []string{"/me/chats?$expand=members&$top=50"})
	if err != nil {
		return "", err
	}
	chatID := chatFor(raw, other)
	if chatID == "" {
		chatID, err = c.createChat(ctx, other)
		if err != nil {
			return "", err
		}
	}
	_, err = c.CreateEntity(ctx, "/chats/"+url.PathEscape(chatID)+"/messages", map[string]any{
		"body": map[string]any{"contentType": "text", "content": text},
	})
	return chatID, err
}

func (c *Client) PostChannel(ctx context.Context, teamID, channelID, text string) (string, string, error) {
	if teamID == "" {
		raw, err := c.Fetch(ctx, []string{"/me/joinedTeams?$select=id,displayName"})
		if err != nil {
			return "", "", err
		}
		teamID = firstID(raw)
		if teamID == "" {
			return "", "", fmt.Errorf("WorkIQ found no joined team")
		}
	}
	if channelID == "" {
		raw, err := c.Fetch(ctx, []string{"/teams/" + url.PathEscape(teamID) + "/channels?$select=id,displayName"})
		if err != nil {
			return "", "", err
		}
		channelID = channelIDFrom(raw)
		if channelID == "" {
			return "", "", fmt.Errorf("WorkIQ found no channel")
		}
	}
	_, err := c.CreateEntity(ctx, fmt.Sprintf("/teams/%s/channels/%s/messages", url.PathEscape(teamID), url.PathEscape(channelID)), map[string]any{
		"body": map[string]any{"contentType": "text", "content": text},
	})
	return teamID, channelID, err
}

func (c *Client) Fetch(ctx context.Context, entityURLs []string) (json.RawMessage, error) {
	return c.call(ctx, "fetch", map[string]any{"entityUrls": entityURLs})
}

func (c *Client) DoAction(ctx context.Context, actionURL string, body any) (json.RawMessage, error) {
	return c.call(ctx, "do_action", map[string]any{"actionUrl": actionURL, "jsonBody": body})
}

func (c *Client) CreateEntity(ctx context.Context, parentURL string, body any) (json.RawMessage, error) {
	return c.call(ctx, "create_entity", map[string]any{"parentUrl": parentURL, "jsonBody": body})
}

func (c *Client) FetchBlob(ctx context.Context, path string) (json.RawMessage, error) {
	return c.call(ctx, "fetch_blob", map[string]any{"path": path})
}

func (c *Client) call(ctx context.Context, name string, args map[string]any) (json.RawMessage, error) {
	if !c.Enabled() {
		return nil, fmt.Errorf("WorkIQ MCP is not configured")
	}
	if strings.TrimSpace(c.Token) == "" {
		return nil, fmt.Errorf("WorkIQ sign-in missing; run login as CharlotteW or BrookeG")
	}
	if err := c.ensureSession(ctx); err != nil {
		return nil, err
	}
	raw, err := c.rpc(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		c.clearSession()
		return nil, fmt.Errorf("WorkIQ %s: %w", name, err)
	}
	if isToolError(raw) {
		return nil, fmt.Errorf("WorkIQ %s: %s", name, truncate([]byte(toolErrorText(raw))))
	}
	return raw, nil
}

func (c *Client) ensureSession(ctx context.Context) error {
	c.mu.Lock()
	ready := c.sessionID != ""
	c.mu.Unlock()
	if ready {
		return nil
	}
	_, err := c.rpc(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "caldova-gcp-agent-v2", "version": "2"},
	})
	if err != nil {
		return fmt.Errorf("WorkIQ initialize: %w", err)
	}
	note, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	req, err := c.newRequest(ctx, note)
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
		return fmt.Errorf("WorkIQ initialized notification failed: %s", resp.Status)
	}
	return nil
}

func (c *Client) rpc(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	req, err := c.newRequest(ctx, body)
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
		c.sessionID = sessionID
		c.mu.Unlock()
	}
	if resp.StatusCode >= 300 {
		c.clearSession()
		return nil, fmt.Errorf("%s", truncate(respBody))
	}
	message := firstJSON(respBody)
	var parsed struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(message, &parsed); err != nil {
		return nil, fmt.Errorf("%s", truncate(respBody))
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("%s", parsed.Error.Message)
	}
	if len(parsed.Result) == 0 {
		return message, nil
	}
	return parsed.Result, nil
}

func (c *Client) newRequest(ctx context.Context, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", protocolVersion)
	c.mu.Lock()
	sessionID := c.sessionID
	c.mu.Unlock()
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return req, nil
}

func (c *Client) clearSession() {
	c.mu.Lock()
	c.sessionID = ""
	c.mu.Unlock()
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

func (c *Client) createChat(ctx context.Context, other string) (string, error) {
	meRaw, err := c.Fetch(ctx, []string{"/me?$select=id"})
	if err != nil {
		return "", err
	}
	otherRaw, err := c.Fetch(ctx, []string{"/users/" + url.PathEscape(other) + "?$select=id"})
	if err != nil {
		return "", err
	}
	meID := findString(meRaw, "id")
	otherID := findString(otherRaw, "id")
	if meID == "" || otherID == "" {
		return "", fmt.Errorf("WorkIQ could not resolve chat members")
	}
	created, err := c.CreateEntity(ctx, "/chats", map[string]any{
		"chatType": "oneOnOne",
		"members":  []any{member(meID), member(otherID)},
	})
	if err != nil {
		return "", err
	}
	id := findString(created, "id")
	if id == "" {
		return "", fmt.Errorf("WorkIQ created a chat without an id")
	}
	return id, nil
}

func member(id string) map[string]any {
	return map[string]any{
		"@odata.type":     "#microsoft.graph.aadUserConversationMember",
		"roles":           []string{"owner"},
		"user@odata.bind": "https://graph.microsoft.com/v1.0/users('" + strings.ReplaceAll(id, "'", "''") + "')",
	}
}

func siteSearchTerm(sitePath string) string {
	sitePath = strings.Trim(sitePath, "/")
	if i := strings.LastIndex(sitePath, "/"); i >= 0 {
		sitePath = sitePath[i+1:]
	}
	return strings.TrimSpace(sitePath)
}

func siteIDFrom(raw json.RawMessage, host, sitePath string) string {
	return walkSite(decodeLoose(raw), strings.ToLower(host), strings.ToLower(strings.TrimRight(sitePath, "/")), strings.ToLower(siteSearchTerm(sitePath)))
}

func walkSite(v any, host, sitePath, name string) string {
	switch t := v.(type) {
	case map[string]any:
		id := asString(t["id"])
		web := strings.ToLower(asString(t["webUrl"]))
		display := strings.ToLower(asString(t["displayName"]))
		siteName := strings.ToLower(asString(t["name"]))
		if id != "" && (strings.Contains(web, host+sitePath) || siteName == name || display == name) {
			return id
		}
		for _, child := range t {
			if found := walkSite(child, host, sitePath, name); found != "" {
				return found
			}
		}
	case []any:
		for _, child := range t {
			if found := walkSite(child, host, sitePath, name); found != "" {
				return found
			}
		}
	}
	return ""
}

func driveRootFrom(raw json.RawMessage) (string, string) {
	return walkDrive(decodeLoose(raw))
}

func walkDrive(v any) (string, string) {
	switch t := v.(type) {
	case map[string]any:
		if root, ok := t["root"].(map[string]any); ok {
			if id, rootID := asString(t["id"]), asString(root["id"]); id != "" && rootID != "" {
				return id, rootID
			}
		}
		for _, child := range t {
			if id, rootID := walkDrive(child); id != "" && rootID != "" {
				return id, rootID
			}
		}
	case []any:
		for _, child := range t {
			if id, rootID := walkDrive(child); id != "" && rootID != "" {
				return id, rootID
			}
		}
	}
	return "", ""
}

func filesFrom(raw json.RawMessage) (string, []m365.DriveItem) {
	var driveID string
	var files []m365.DriveItem
	collectFiles(decodeLoose(raw), &files, &driveID)
	return driveID, files
}

func collectFiles(v any, files *[]m365.DriveItem, driveID *string) {
	switch t := v.(type) {
	case map[string]any:
		if _, ok := t["file"]; ok {
			if name, _ := t["name"].(string); name != "" {
				item := m365.DriveItem{ID: asString(t["id"]), Name: name, Size: asInt64(t["size"]), WebURL: asString(t["webUrl"])}
				if file, ok := t["file"].(map[string]any); ok {
					item.File = &struct {
						MimeType string `json:"mimeType"`
					}{MimeType: asString(file["mimeType"])}
				}
				if ref, ok := t["parentReference"].(map[string]any); ok && *driveID == "" {
					*driveID = asString(ref["driveId"])
				}
				*files = append(*files, item)
			}
		}
		for _, child := range t {
			collectFiles(child, files, driveID)
		}
	case []any:
		for _, child := range t {
			collectFiles(child, files, driveID)
		}
	}
}

func chatFor(raw json.RawMessage, email string) string {
	return walkChat(decodeLoose(raw), email)
}

func walkChat(v any, email string) string {
	switch t := v.(type) {
	case map[string]any:
		if id := asString(t["id"]); id != "" && mentions(t["members"], email) {
			return id
		}
		for _, child := range t {
			if id := walkChat(child, email); id != "" {
				return id
			}
		}
	case []any:
		for _, child := range t {
			if id := walkChat(child, email); id != "" {
				return id
			}
		}
	}
	return ""
}

func mentions(v any, email string) bool {
	blob, _ := json.Marshal(v)
	text := strings.ToLower(string(blob))
	email = strings.ToLower(email)
	local := email
	if i := strings.Index(email, "@"); i > 0 {
		local = email[:i]
	}
	return strings.Contains(text, email) || strings.Contains(text, local)
}

func firstID(raw json.RawMessage) string {
	return findString(raw, "id")
}

func channelIDFrom(raw json.RawMessage) string {
	var general, first string
	walkChannels(decodeLoose(raw), &general, &first)
	if general != "" {
		return general
	}
	return first
}

func walkChannels(v any, general, first *string) {
	switch t := v.(type) {
	case map[string]any:
		if id, name := asString(t["id"]), asString(t["displayName"]); id != "" && name != "" {
			if *first == "" {
				*first = id
			}
			if strings.EqualFold(name, "General") {
				*general = id
			}
		}
		for _, child := range t {
			walkChannels(child, general, first)
		}
	case []any:
		for _, child := range t {
			walkChannels(child, general, first)
		}
	}
}

func findString(raw json.RawMessage, key string) string {
	return walkString(decodeLoose(raw), key)
}

func walkString(v any, key string) string {
	switch t := v.(type) {
	case map[string]any:
		if s := asString(t[key]); s != "" {
			return s
		}
		for _, child := range t {
			if s := walkString(child, key); s != "" {
				return s
			}
		}
	case []any:
		for _, child := range t {
			if s := walkString(child, key); s != "" {
				return s
			}
		}
	}
	return ""
}

func decodeLoose(raw json.RawMessage) any {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	if text, ok := contentText(v); ok {
		var inner any
		if json.Unmarshal([]byte(text), &inner) == nil {
			return inner
		}
	}
	return v
}

func contentText(v any) (string, bool) {
	root, _ := v.(map[string]any)
	content, _ := root["content"].([]any)
	for _, item := range content {
		entry, _ := item.(map[string]any)
		if text := asString(entry["text"]); text != "" {
			return text, true
		}
	}
	return "", false
}

func isToolError(raw json.RawMessage) bool {
	var result struct {
		IsError bool `json:"isError"`
	}
	_ = json.Unmarshal(raw, &result)
	return result.IsError
}

func toolErrorText(raw json.RawMessage) string {
	if text, ok := contentText(decodeLoose(raw)); ok {
		return text
	}
	return string(raw)
}

func firstJSON(body []byte) []byte {
	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] == '{' || body[0] == '[' {
		return body
	}
	var data bytes.Buffer
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("data:")) {
			data.Write(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))))
		}
	}
	if data.Len() > 0 {
		return data.Bytes()
	}
	return body
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	default:
		return 0
	}
}

func isText(item m365.DriveItem) bool {
	if item.File == nil {
		return false
	}
	mime := strings.ToLower(item.File.MimeType)
	return strings.HasPrefix(mime, "text/") || strings.Contains(mime, "json") || strings.Contains(mime, "csv")
}

func truncate(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 400 {
		return s[:400]
	}
	return s
}
