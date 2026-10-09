package purview

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestFailOpenDoesNotAllowPolicyBlock(t *testing.T) {
	c := &Client{Guard: PurviewGuard{Enabled: true, FailClosed: false, loaded: true}}
	got := c.closed(Decide(200, []byte(`{"policyActions":[{"restrictionAction":"block"}]}`)))
	if got.Allowed {
		t.Fatal("fail-open must not override a policy block")
	}
}

func TestDecideBlocksRestrictAccess(t *testing.T) {
	body := []byte(`{"policyActions":[{"@odata.type":"#microsoft.graph.dlpActionInfo","action":"restrictAccess"}]}`)
	got := Decide(200, body)
	if got.Allowed {
		t.Fatalf("expected block, got %#v", got)
	}
}

func TestDecideAllowsEmptyPolicyActions(t *testing.T) {
	got := Decide(200, []byte(`{"policyActions":[],"protectionScopeState":"notModified"}`))
	if !got.Allowed {
		t.Fatalf("expected allow, got %#v", got)
	}
}

func TestDecideFailsClosedOnError(t *testing.T) {
	got := Decide(403, []byte(`{"error":{"code":"Forbidden"}}`))
	if got.Allowed {
		t.Fatalf("expected fail closed, got %#v", got)
	}
}

func TestPayloadPairsPromptAndResponse(t *testing.T) {
	c := Client{AppID: "app", AppName: "Caldova", BlueprintID: "blueprint", AgentIdentityID: "agent"}
	prompt := c.payload("uploadText", "send the file", "corr", 1, true)
	response := c.payload("downloadText", "sent the file", "corr", 2, false)
	promptEntry := prompt["contentToProcess"].(map[string]any)["contentEntries"].([]any)[0].(map[string]any)
	responseEntry := response["contentToProcess"].(map[string]any)["contentEntries"].([]any)[0].(map[string]any)
	if promptEntry["correlationId"] != responseEntry["correlationId"] {
		t.Fatal("prompt and response must share a correlation id")
	}
	if _, ok := responseEntry["content"]; ok {
		t.Fatal("metadata-only payload must not include content text")
	}
	activity := c.activityPayload("downloadText", "corr", 2)
	if _, ok := activity["contentToProcess"]; ok {
		t.Fatal("contentActivities must not use the processContent wrapper")
	}
	meta, ok := activity["contentMetadata"].(map[string]any)
	if !ok {
		t.Fatal("contentActivities requires contentMetadata")
	}
	activityEntry := meta["contentEntries"].([]any)[0].(map[string]any)
	if _, ok := activityEntry["content"]; ok {
		t.Fatal("contentActivities must not include prompt or response text")
	}
	if activityEntry["correlationId"] != promptEntry["correlationId"] {
		t.Fatal("activity must keep the prompt correlation id")
	}
	if prompt["contentToProcess"].(map[string]any)["activityMetadata"].(map[string]any)["activity"] != "uploadText" {
		t.Fatal("prompt activity")
	}
	if response["contentToProcess"].(map[string]any)["activityMetadata"].(map[string]any)["activity"] != "downloadText" {
		t.Fatal("response activity")
	}
}

func TestDecideNoContentFailsClosed(t *testing.T) {
	got := Decide(204, nil)
	if got.Allowed {
		t.Fatalf("expected fail closed, got %#v", got)
	}
}

func TestProcessContentEndpointIsDelegatedBeta(t *testing.T) {
	if processContentEndpoint() != "https://graph.microsoft.com/beta/me/dataSecurityAndGovernance/processContent" {
		t.Fatal(processContentEndpoint())
	}
}

func TestOversizedContentIsRejectedBeforeGraph(t *testing.T) {
	c := &Client{
		Token: "token",
		Guard: PurviewGuard{Enabled: true, FailClosed: true, Timeout: time.Second, loaded: true},
	}
	got, err := c.processContent(context.Background(), "uploadText", strings.Repeat("x", maxContentChars+1), "corr", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Allowed {
		t.Fatalf("expected reject, got %#v", got)
	}
}

func TestDisabledGuardSkipsProcessContent(t *testing.T) {
	c := &Client{Guard: PurviewGuard{Enabled: false, loaded: true}}
	got, err := c.Inspect(context.Background(), "uploadText", "send the file", "corr", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Allowed || !strings.Contains(got.Reason, "PURVIEW_DLP_ENABLED") {
		t.Fatalf("expected disabled skip, got %#v", got)
	}
}

func TestPayloadKeepsFullTextAndName(t *testing.T) {
	c := Client{AppName: "Caldova"}
	text := strings.Repeat("a", 12001)
	payload := c.payload("uploadText", text, "corr", 1, true)
	entry := payload["contentToProcess"].(map[string]any)["contentEntries"].([]any)[0].(map[string]any)
	if entry["name"] == "" {
		t.Fatal("processContent name is required")
	}
	if entry["isTruncated"] != false {
		t.Fatal("full text must not be marked truncated")
	}
	data := entry["content"].(map[string]any)["data"].(string)
	if data != text {
		t.Fatal("processContent must send the full text under the skill limit")
	}
}
