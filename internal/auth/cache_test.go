package auth

import (
	"strings"
	"testing"
)

func TestHostedCacheDropsRegistrationToken(t *testing.T) {
	raw := `{
	  "entries": {
	    "register-caldova": {"access_token": "admin", "refresh_token": "admin-refresh"},
	    "runtime:client": {"access_token": "graph", "refresh_token": "graph-refresh"},
	    "workiq:user@example.com": {"access_token": "workiq", "refresh_token": "workiq-refresh"},
	    "other": {"access_token": "nope", "refresh_token": "nope-refresh"}
	  }
	}`
	filtered, err := HostedCache(raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(filtered, "admin") || strings.Contains(filtered, "other") || strings.Contains(filtered, "nope") {
		t.Fatalf("hosted cache kept a non-user token: %s", filtered)
	}
	if !strings.Contains(filtered, "graph-refresh") || !strings.Contains(filtered, "workiq-refresh") {
		t.Fatalf("hosted cache dropped a test-user token: %s", filtered)
	}
	if strings.Contains(filtered, `"access_token":"graph"`) {
		t.Fatalf("hosted cache kept an access token: %s", filtered)
	}
}

func TestHostedCacheKeepsBothGraphUsers(t *testing.T) {
	raw := `{
	  "entries": {
	    "runtime:client:charlottew@caldova56317036.onmicrosoft.com": {"refresh_token": "charlotte-refresh"},
	    "runtime:client:brookeg@caldova56317036.onmicrosoft.com": {"refresh_token": "brooke-refresh"}
	  }
	}`
	filtered, err := HostedCache(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(filtered, "charlotte-refresh") || !strings.Contains(filtered, "brooke-refresh") {
		t.Fatalf("hosted cache dropped a Graph user slot: %s", filtered)
	}
}
