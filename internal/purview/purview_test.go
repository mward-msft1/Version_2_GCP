package purview

import "testing"

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

func TestDecideNoContentAllowed(t *testing.T) {
	got := Decide(204, nil)
	if !got.Allowed {
		t.Fatalf("expected allow, got %#v", got)
	}
}
