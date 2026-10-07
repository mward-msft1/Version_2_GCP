package a365obs

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Step is one agent function call exported as an execute_tool span.
type Step struct {
	Name   string
	Detail string
	OK     bool
}

// Run is one agent invocation. ExportRun emits its root invoke_agent span
// through the Microsoft OpenTelemetry Distro.
type Run struct {
	ActingUser     string
	UserID         string
	ConversationID string
	FileName       string
	Input          string
	Output         string
	Steps          []Step
}

// Result is the Agent 365 ingestion response. HTTP 200 is not proof that a
// span was accepted; Summary includes each sink status.
type Result struct {
	Status    int             `json:"status"`
	SpanCount int             `json:"spanCount"`
	Body      json.RawMessage `json:"body,omitempty"`
	Summary   string          `json:"summary"`
}

func summarize(status int, raw []byte) string {
	var parsed struct {
		PartialSuccess struct {
			RejectedSpans int    `json:"rejectedSpans"`
			ErrorMessage  string `json:"errorMessage"`
		} `json:"partialSuccess"`
		Results []struct {
			Sinks map[string]struct {
				Status string `json:"status"`
				Reason string `json:"reason"`
			} `json:"sinks"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Sprintf("otel http %d", status)
	}
	sent, rejected := 0, 0
	reason := ""
	for _, item := range parsed.Results {
		for _, sink := range item.Sinks {
			switch sink.Status {
			case "sent":
				sent++
			case "rejected":
				rejected++
				if reason == "" {
					reason = sink.Reason
				}
			}
		}
	}
	summary := fmt.Sprintf("otel http %d, sent %d, rejected %d", status, sent, rejected)
	if parsed.PartialSuccess.RejectedSpans > 0 {
		summary += fmt.Sprintf(", dropped %d", parsed.PartialSuccess.RejectedSpans)
	}
	if reason != "" {
		summary += ", reason " + reason
	}
	if parsed.PartialSuccess.ErrorMessage != "" {
		summary += ", " + parsed.PartialSuccess.ErrorMessage
	}
	return summary
}

func messages(role, content string) string {
	encoded, _ := json.Marshal([]map[string]string{{"role": role, "content": truncate(content, 4000)}})
	return string(encoded)
}

func first(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}
