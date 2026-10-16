package aigateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatUsesApiKeyAndModelName(t *testing.T) {
	var gotPath, gotKey, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("Api-Key")
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"model":"gemini-2.5-flash"`) {
			gotModel = "gemini-2.5-flash"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}]}`))
	}))
	defer srv.Close()

	got, err := (Client{Host: srv.URL, APIKey: "test-key", Model: DefaultModel, HTTP: srv.Client()}).Chat(context.Background(), "ping")
	if err != nil {
		t.Fatal(err)
	}
	if got != "pong" || gotPath != "/default/models/openai/v1/chat/completions" || gotKey != "test-key" || gotModel != DefaultModel {
		t.Fatalf("path=%s key=%s model=%s reply=%s", gotPath, gotKey, gotModel, got)
	}
}

func TestBaseURL(t *testing.T) {
	got := BaseURL("https://merry-meteor-4379.eastus2-01.ai.gateway.azure.com/")
	want := "https://merry-meteor-4379.eastus2-01.ai.gateway.azure.com/default/models/openai/v1"
	if got != want {
		t.Fatalf("got %s", got)
	}
	if BaseURL(want) != want {
		t.Fatal("suffix was doubled")
	}
}

func TestChatRequiresKey(t *testing.T) {
	_, err := (Client{}).Chat(context.Background(), "ping")
	if err == nil || !strings.Contains(err.Error(), "AI_GATEWAY_API_KEY") {
		t.Fatalf("got %v", err)
	}
}
