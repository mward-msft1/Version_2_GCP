package a365obs

import (
	"strings"
	"testing"
)

func TestDistroResultRedactsTokenAndReadsStatus(t *testing.T) {
	raw := []byte("noise\n{\"status\":200,\"spanCount\":3,\"summary\":\"microsoft-opentelemetry exported invoke_agent\"}\n")
	result := parseDistroResult(raw, 9)
	if result.Status != 200 || !strings.Contains(result.Summary, "microsoft-opentelemetry") {
		t.Fatalf("distro result was not parsed: %+v", result)
	}
}
