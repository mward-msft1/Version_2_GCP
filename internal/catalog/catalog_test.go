package catalog

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInvokeListsAndCallsCatalogServer(t *testing.T) {
	var sawInit, sawList, sawCall bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("Authorization") == "" {
			t.Error("missing bearer token")
		}
		w.Header().Set("Mcp-Session-Id", "session")
		text := string(body)
		switch {
		case strings.Contains(text, "initialize"):
			sawInit = true
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26"}}`))
		case strings.Contains(text, "notifications/initialized"):
			w.WriteHeader(http.StatusAccepted)
		case strings.Contains(text, "tools/list"):
			sawList = true
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"getOnedrive"}]}}`))
		case strings.Contains(text, "tools/call"):
			sawCall = true
			if !strings.Contains(text, "getOnedrive") {
				t.Errorf("unexpected call: %s", text)
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":3,"result":{"drive":"ok"}}`))
		default:
			t.Errorf("unexpected body: %s", text)
		}
	}))
	defer server.Close()

	client := &Client{
		Servers: []Server{{Name: "mcp_OneDriveRemoteServer", Alias: "onedrive", URL: server.URL, Audience: "aud"}},
		Tokens:  map[string]string{"aud": "token"},
	}
	listed, err := client.Invoke(context.Background(), "onedrive", Request{Tool: "list"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listed.Output, "getOnedrive") {
		t.Fatalf("list output: %s", listed.Output)
	}
	called, err := client.Invoke(context.Background(), "onedrive", Request{Tool: "getOnedrive"})
	if err != nil {
		t.Fatal(err)
	}
	if !sawInit || !sawList || !sawCall || !strings.Contains(called.Output, "drive") {
		t.Fatalf("init=%t list=%t call=%t output=%s", sawInit, sawList, sawCall, called.Output)
	}
}

func TestInvokeStopsWhenPurviewBlocks(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := &Client{
		Servers: []Server{{Name: "mcp_WordServer", Alias: "word", URL: server.URL, Audience: "aud"}},
		Tokens:  map[string]string{"aud": "token"},
		Inspector: func(context.Context, string) (bool, string, error) {
			return false, "credit card", nil
		},
	}
	result, err := client.Invoke(context.Background(), "word", Request{Tool: "WordCreateNewDocument", Arguments: map[string]any{"contentInHtml": "4111"}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Blocked || called {
		t.Fatalf("blocked=%t called=%t", result.Blocked, called)
	}
}

func TestByAliasIncludesUserServer(t *testing.T) {
	server, err := ByAlias(Servers, "user")
	if err != nil {
		t.Fatal(err)
	}
	if server.Name != "mcp_MeServer" || server.Audience != "147dc821-b413-44c0-8009-1a3098378012" {
		t.Fatalf("%+v", server)
	}
}

func TestFirstJSONReadsSSEEvent(t *testing.T) {
	body := []byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"ok\":true}}\n")
	got := string(firstJSON(body))
	if !strings.Contains(got, `"ok":true`) {
		t.Fatalf("sse payload not extracted: %s", got)
	}
}
