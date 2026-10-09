package workiq

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHostedToolsForBothUsers(t *testing.T) {
	var sawMailCC, sawCharlotteChat, sawChannel bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Mcp-Session-Id", "session")
		if !strings.Contains(string(body), "tools/call") {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26"}}`))
			return
		}
		var call struct {
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &call)
		switch call.Params.Name {
		case "fetch":
			urls, _ := call.Params.Arguments["entityUrls"].([]any)
			path, _ := urls[0].(string)
			switch {
			case strings.HasPrefix(path, "/sites?search="):
				writeResult(w, `{"value":[{"id":"site,with,commas","displayName":"DocSite","name":"DocSite","webUrl":"https://caldova56317036.sharepoint.com/sites/DocSite"}]}`)
				return
			case strings.Contains(path, "/drive?"):
				if strings.Contains(path, "%2C") || strings.Contains(path, "%2c") {
					http.Error(w, "encoded site id", http.StatusBadRequest)
					return
				}
				writeResult(w, `{"id":"drive","name":"Documents","root":{"id":"root"}}`)
				return
			case strings.Contains(path, "/drives/drive/items/root/children"):
				writeResult(w, `{"value":[{"id":"item","name":"sensitive.txt","size":4,"webUrl":"https://example","file":{"mimeType":"text/plain"},"parentReference":{"driveId":"drive"}}]}`)
				return
			}
			if strings.Contains(path, "/me/chats") {
				writeResult(w, `{"value":[{"id":"chat-1","members":[{"displayName":"CharlotteW"}]}]}`)
				return
			}
		case "fetch_blob":
			writeResult(w, `{"base64Content":"dGVzdA==","name":"sensitive.txt"}`)
			return
		case "do_action":
			if call.Params.Arguments["actionUrl"] == "/me/sendMail" {
				blob, _ := json.Marshal(call.Params.Arguments["jsonBody"])
				if strings.Contains(string(blob), "CharlotteW@Caldova56317036.onmicrosoft.com") && strings.Contains(string(blob), "mward042@gmail.com") {
					sawMailCC = true
				}
			}
			writeResult(w, `{"webUrl":"https://example/link"}`)
			return
		case "create_entity":
			parent, _ := call.Params.Arguments["parentUrl"].(string)
			blob, _ := json.Marshal(call.Params.Arguments["jsonBody"])
			if strings.Contains(parent, "/chats/chat-1/messages") && strings.Contains(string(blob), "CharlotteW") {
				sawCharlotteChat = true
			}
			if strings.Contains(parent, "/channels/general/messages") && strings.Contains(string(blob), "BrookeG") && strings.Contains(string(blob), "CharlotteW") {
				sawChannel = true
			}
			writeResult(w, `{"id":"created"}`)
			return
		}
		http.Error(w, "unexpected tool", http.StatusBadRequest)
	}))
	defer server.Close()

	client := &Client{URL: server.URL, Token: "user-token", HTTP: server.Client()}
	driveID, files, err := client.ListDocuments(context.Background(), "caldova56317036.sharepoint.com", "/sites/DocSite")
	if err != nil || driveID != "drive" || len(files) != 1 {
		t.Fatalf("list: %v %s %#v", err, driveID, files)
	}
	file, err := client.Download(context.Background(), driveID, files[0])
	if err != nil || string(file.Bytes) != "test" {
		t.Fatalf("download: %v %q", err, file.Bytes)
	}
	err = client.SendMail(context.Background(), "mward042@gmail.com", []string{"CharlotteW@Caldova56317036.onmicrosoft.com", "BrookeG@Caldova56317036.onmicrosoft.com"}, "subject", "body", file)
	if err != nil || !sawMailCC {
		t.Fatalf("mail: %v cc=%v", err, sawMailCC)
	}
	if _, err = client.PostChat(context.Background(), "CharlotteW@Caldova56317036.onmicrosoft.com", messageFor("CharlotteW")); err != nil || !sawCharlotteChat {
		t.Fatalf("chat: %v saw=%v", err, sawCharlotteChat)
	}
	if _, _, err = client.PostChannel(context.Background(), "team", "general", "DLP test for CharlotteW, BrookeG"); err != nil || !sawChannel {
		t.Fatalf("channel: %v saw=%v", err, sawChannel)
	}
}

func messageFor(user string) string {
	return "DLP test for " + user
}

func writeResult(w http.ResponseWriter, text string) {
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"result": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": text}},
		},
	}
	_ = json.NewEncoder(w).Encode(payload)
}

func TestChatMatchesEitherUser(t *testing.T) {
	raw := json.RawMessage(`{"content":[{"type":"text","text":"{\"value\":[{\"id\":\"brooke-chat\",\"members\":[{\"displayName\":\"BrookeG\"}]}]}"}]}`)
	if chatFor(raw, "BrookeG@Caldova56317036.onmicrosoft.com") != "brooke-chat" {
		t.Fatal("expected Brooke chat")
	}
}
