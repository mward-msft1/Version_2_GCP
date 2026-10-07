package m365

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

const maxAttachmentBytes = 3 * 1024 * 1024

type Client struct {
	Token string
	HTTP  *http.Client
}

type DriveItem struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	WebURL string `json:"webUrl"`
	Folder *struct {
		ChildCount int `json:"childCount"`
	} `json:"folder"`
	File *struct {
		MimeType string `json:"mimeType"`
	} `json:"file"`
}

type FilePayload struct {
	Item    DriveItem
	DriveID string
	Bytes   []byte
	Snippet string
	Link    string
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) Me(ctx context.Context) (string, string, error) {
	var me struct {
		ID   string `json:"id"`
		Mail string `json:"mail"`
		UPN  string `json:"userPrincipalName"`
	}
	if err := c.get(ctx, "https://graph.microsoft.com/v1.0/me?$select=id,mail,userPrincipalName", &me); err != nil {
		return "", "", err
	}
	email := me.Mail
	if email == "" {
		email = me.UPN
	}
	return me.ID, email, nil
}

func (c *Client) ListDocuments(ctx context.Context, host, sitePath string) (string, []DriveItem, error) {
	driveID, err := c.documentsDrive(ctx, host, sitePath)
	if err != nil {
		return "", nil, err
	}
	var files []DriveItem
	listURL := fmt.Sprintf("https://graph.microsoft.com/v1.0/drives/%s/root/children?$select=id,name,size,webUrl,folder,file&$top=200", url.PathEscape(driveID))
	for listURL != "" && len(files) < 1000 {
		var page struct {
			Value []DriveItem `json:"value"`
			Next  string      `json:"@odata.nextLink"`
		}
		if err := c.get(ctx, listURL, &page); err != nil {
			return "", nil, err
		}
		for _, item := range page.Value {
			if item.File != nil {
				files = append(files, item)
			}
		}
		listURL = page.Next
	}
	return driveID, files, nil
}

// FindDocument searches the DocSite library for an exact file name. The root
// children page can omit later files, so a named test document must not fall
// back to the first listed file.
func (c *Client) FindDocument(ctx context.Context, host, sitePath, name string) (string, DriveItem, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", DriveItem{}, fmt.Errorf("document name is required")
	}
	driveID, err := c.documentsDrive(ctx, host, sitePath)
	if err != nil {
		return "", DriveItem{}, err
	}
	query := strings.ReplaceAll(name, "'", "''")
	encoded := strings.ReplaceAll(url.QueryEscape(query), "+", "%20")
	searchURL := fmt.Sprintf("https://graph.microsoft.com/v1.0/drives/%s/root/search(q='%s')?$select=id,name,size,webUrl,file,folder", url.PathEscape(driveID), encoded)
	var items struct {
		Value []DriveItem `json:"value"`
	}
	if err := c.get(ctx, searchURL, &items); err != nil {
		return "", DriveItem{}, err
	}
	for _, item := range items.Value {
		if item.File != nil && strings.EqualFold(item.Name, name) {
			return driveID, item, nil
		}
	}
	return "", DriveItem{}, fmt.Errorf("document %s was not found in %s%s", name, host, sitePath)
}

func (c *Client) documentsDrive(ctx context.Context, host, sitePath string) (string, error) {
	var site struct {
		ID string `json:"id"`
	}
	siteURL := fmt.Sprintf("https://graph.microsoft.com/v1.0/sites/%s:%s", host, sitePath)
	if err := c.get(ctx, siteURL, &site); err != nil {
		return "", err
	}
	var drives struct {
		Value []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"value"`
	}
	if err := c.get(ctx, "https://graph.microsoft.com/v1.0/sites/"+url.PathEscape(site.ID)+"/drives?$select=id,name", &drives); err != nil {
		return "", err
	}
	for _, drive := range drives.Value {
		if strings.EqualFold(drive.Name, "Documents") {
			return drive.ID, nil
		}
	}
	if len(drives.Value) > 0 {
		return drives.Value[0].ID, nil
	}
	return "", fmt.Errorf("no document library found at %s%s", host, sitePath)
}

func (c *Client) PrepareFile(ctx context.Context, driveID string, item DriveItem) (FilePayload, error) {
	payload := FilePayload{Item: item, DriveID: driveID, Snippet: item.Name}
	if item.Size > 0 && item.Size <= maxAttachmentBytes {
		raw, err := c.getBytes(ctx, fmt.Sprintf("https://graph.microsoft.com/v1.0/drives/%s/items/%s/content", url.PathEscape(driveID), url.PathEscape(item.ID)))
		if err != nil {
			return payload, err
		}
		payload.Bytes = raw
		payload.Snippet = ContentSnippet(item, raw)
	}
	var link struct {
		Link struct {
			WebURL string `json:"webUrl"`
		} `json:"link"`
	}
	err := c.post(ctx, fmt.Sprintf("https://graph.microsoft.com/v1.0/drives/%s/items/%s/createLink", url.PathEscape(driveID), url.PathEscape(item.ID)), map[string]any{
		"type":  "view",
		"scope": "organization",
	}, &link)
	if err != nil {
		payload.Link = item.WebURL
		return payload, nil
	}
	payload.Link = link.Link.WebURL
	return payload, nil
}

func (c *Client) SendMail(ctx context.Context, to string, cc []string, subject, bodyText string, file FilePayload) error {
	attachment := map[string]any{}
	if len(file.Bytes) > 0 {
		attachment = map[string]any{
			"@odata.type":  "#microsoft.graph.fileAttachment",
			"name":         file.Item.Name,
			"contentBytes": base64.StdEncoding.EncodeToString(file.Bytes),
		}
	}
	message := map[string]any{
		"subject": subject,
		"body": map[string]any{
			"contentType": "Text",
			"content":     bodyText,
		},
		"toRecipients": []any{
			map[string]any{"emailAddress": map[string]any{"address": to}},
		},
	}
	if len(cc) > 0 {
		var copied []any
		for _, address := range cc {
			if strings.EqualFold(address, to) {
				continue
			}
			copied = append(copied, map[string]any{"emailAddress": map[string]any{"address": address}})
		}
		if len(copied) > 0 {
			message["ccRecipients"] = copied
		}
	}
	if len(attachment) > 0 {
		message["attachments"] = []any{attachment}
	}
	return c.post(ctx, "https://graph.microsoft.com/v1.0/me/sendMail", map[string]any{
		"message":         message,
		"saveToSentItems": true,
	}, nil)
}

func (c *Client) PostChat(ctx context.Context, actingUserID, otherUserID, html string) (string, error) {
	var chat struct {
		ID string `json:"id"`
	}
	err := c.post(ctx, "https://graph.microsoft.com/v1.0/chats", map[string]any{
		"chatType": "oneOnOne",
		"members": []any{
			member(actingUserID),
			member(otherUserID),
		},
	}, &chat)
	if err != nil {
		return "", err
	}
	err = c.post(ctx, "https://graph.microsoft.com/v1.0/chats/"+url.PathEscape(chat.ID)+"/messages", map[string]any{
		"body": map[string]any{"contentType": "html", "content": html},
	}, nil)
	return chat.ID, err
}

func (c *Client) PostChannel(ctx context.Context, teamID, channelID, html string) (string, string, error) {
	if teamID == "" || channelID == "" {
		var teams struct {
			Value []struct {
				ID string `json:"id"`
			} `json:"value"`
		}
		if err := c.get(ctx, "https://graph.microsoft.com/v1.0/me/joinedTeams?$select=id", &teams); err != nil {
			return "", "", err
		}
		if len(teams.Value) == 0 {
			return "", "", fmt.Errorf("acting user has no joined Teams team")
		}
		teamID = teams.Value[0].ID
		var channels struct {
			Value []struct {
				ID string `json:"id"`
			} `json:"value"`
		}
		if err := c.get(ctx, "https://graph.microsoft.com/v1.0/teams/"+url.PathEscape(teamID)+"/channels?$select=id", &channels); err != nil {
			return "", "", err
		}
		if len(channels.Value) == 0 {
			return "", "", fmt.Errorf("team %s has no channels", teamID)
		}
		channelID = channels.Value[0].ID
	}
	err := c.post(ctx, fmt.Sprintf("https://graph.microsoft.com/v1.0/teams/%s/channels/%s/messages", url.PathEscape(teamID), url.PathEscape(channelID)), map[string]any{
		"body": map[string]any{"contentType": "html", "content": html},
	}, nil)
	return teamID, channelID, err
}

func (c *Client) UserID(ctx context.Context, email string) (string, error) {
	var user struct {
		ID string `json:"id"`
	}
	err := c.get(ctx, "https://graph.microsoft.com/v1.0/users/"+url.PathEscape(email)+"?$select=id", &user)
	return user.ID, err
}

func member(id string) map[string]any {
	bind := "https://graph.microsoft.com/v1.0/users('" + strings.ReplaceAll(id, "'", "''") + "')"
	if strings.Contains(id, "@") {
		bind = "https://graph.microsoft.com/v1.0/users/" + url.PathEscape(id)
	}
	return map[string]any{
		"@odata.type":     "#microsoft.graph.aadUserConversationMember",
		"roles":           []string{"owner"},
		"user@odata.bind": bind,
	}
}

func isText(item DriveItem) bool {
	if item.File == nil {
		return false
	}
	mime := strings.ToLower(item.File.MimeType)
	return strings.HasPrefix(mime, "text/") || strings.Contains(mime, "json") || strings.Contains(mime, "csv")
}

var wordText = regexp.MustCompile(`(?s)<w:t[^>]*>(.*?)</w:t>`)

// ContentSnippet returns text Purview can evaluate. Word documents are unzipped
// so published DLP policies see the document text, not only the file name.
func ContentSnippet(item DriveItem, raw []byte) string {
	if isText(item) {
		return item.Name + "\n" + string(raw)
	}
	if text := docxText(item.Name, raw); text != "" {
		return item.Name + "\n" + text
	}
	return item.Name
}

func docxText(name string, raw []byte) string {
	if !strings.HasSuffix(strings.ToLower(name), ".docx") || len(raw) == 0 {
		return ""
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return ""
	}
	for _, file := range zr.File {
		if file.Name != "word/document.xml" {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			return ""
		}
		body, err := io.ReadAll(io.LimitReader(rc, 1<<20))
		rc.Close()
		if err != nil {
			return ""
		}
		parts := wordText.FindAllSubmatch(body, -1)
		var b strings.Builder
		for _, part := range parts {
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(html.UnescapeString(string(part[1])))
			if b.Len() > 12000 {
				break
			}
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}

func (c *Client) get(ctx context.Context, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("GET %s failed: %s", endpoint, truncate(body))
	}
	if out == nil || len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	return json.Unmarshal(body, out)
}

func (c *Client) getBytes(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxAttachmentBytes+1))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s failed: %s", endpoint, truncate(body))
	}
	if len(body) > maxAttachmentBytes {
		return nil, fmt.Errorf("file exceeds %d byte attachment limit", maxAttachmentBytes)
	}
	return body, nil
}

func (c *Client) post(ctx context.Context, endpoint string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("POST %s failed: %s", endpoint, truncate(respBody))
	}
	if out == nil || len(bytes.TrimSpace(respBody)) == 0 {
		return nil
	}
	return json.Unmarshal(respBody, out)
}

func truncate(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 500 {
		return s[:500]
	}
	return s
}
