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

func TestDecideNoContentAllowed(t *testing.T) {
	got := Decide(204, nil)
	if !got.Allowed {
		t.Fatalf("expected allow, got %#v", got)
	}
}
