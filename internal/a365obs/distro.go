package a365obs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mward-msft1/Version_2_GCP/internal/config"
)

// ExportRun records one agent invocation with the Microsoft OpenTelemetry Distro
// and exports it to Agent 365. The distro has no Go package, so this process
// acquires the app-only token and the Python distro creates the spans.
func ExportRun(ctx context.Context, cfg config.Config, run Run) (Result, error) {
	if cfg.AgentIdentityID == "" || cfg.BlueprintID == "" {
		return Result{}, fmt.Errorf("agent identity is not loaded; cannot export OpenTelemetry")
	}
	script, err := distroScript()
	if err != nil {
		return Result{}, err
	}
	python, pythonArgs, err := pythonCommand(ctx)
	if err != nil {
		return Result{}, err
	}
	token, err := observabilityTokenReady(ctx, cfg)
	if err != nil {
		return Result{}, err
	}
	payload, err := json.Marshal(map[string]any{
		"agentId":        cfg.AgentIdentityID,
		"tenantId":       cfg.TenantID,
		"blueprintId":    cfg.BlueprintID,
		"agentName":      cfg.AgentName,
		"description":    cfg.Description,
		"actingUser":     run.ActingUser,
		"userId":         first(run.UserID, "unknown"),
		"conversationId": first(run.ConversationID, "caldova-"+strings.ToLower(run.ActingUser)),
		"input":          first(run.Input, "Run the DLP test. Acting user: "+run.ActingUser),
		"output":         first(run.Output, "Completed the Caldova GCP Agent Version 2 test for "+run.ActingUser+"."),
		"steps":          exportSteps(run.Steps),
	})
	if err != nil {
		return Result{}, err
	}
	exportCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(exportCtx, python, append(pythonArgs, script)...)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Env = append(os.Environ(),
		"A365_OBSERVABILITY_TOKEN="+token,
		"ENABLE_OBSERVABILITY=true",
		"ENABLE_A365_OBSERVABILITY_EXPORTER=true",
		"A365_USE_S2S_ENDPOINT=true",
	)
	out, err := cmd.CombinedOutput()
	out = bytes.ReplaceAll(out, []byte(token), []byte("[redacted]"))
	result := parseDistroResult(out, 2+len(run.Steps))
	if err != nil {
		if result.Summary == "" {
			result.Summary = strings.TrimSpace(string(out))
		}
		return result, fmt.Errorf("microsoft-opentelemetry export failed: %s", truncate(result.Summary, 400))
	}
	if result.Status < 200 || result.Status >= 300 {
		return result, fmt.Errorf("microsoft-opentelemetry export failed: %s", truncate(result.Summary, 400))
	}
	return result, nil
}

func exportSteps(steps []Step) []map[string]any {
	exported := make([]map[string]any, 0, len(steps))
	for _, step := range steps {
		exported = append(exported, map[string]any{
			"name":   step.Name,
			"detail": truncate(step.Detail, 2000),
			"ok":     step.OK,
		})
	}
	return exported
}

func parseDistroResult(out []byte, spanCount int) Result {
	text := strings.TrimSpace(string(out))
	if text == "" {
		return Result{SpanCount: spanCount}
	}
	var result Result
	parsed := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var candidate Result
		if err := json.Unmarshal([]byte(line), &candidate); err != nil || candidate.Summary == "" && candidate.Status == 0 {
			continue
		}
		result = candidate
		parsed = true
	}
	if !parsed {
		return Result{SpanCount: spanCount, Summary: truncate(text, 400)}
	}
	if result.SpanCount == 0 {
		result.SpanCount = spanCount
	}
	return result
}

// VerifyPlatform imports google-adk, google-cloud-aiplatform, and the Agent 365 SDK.
func VerifyPlatform(ctx context.Context) (string, error) {
	script, err := distroScript()
	if err != nil {
		return "", err
	}
	python, pythonArgs, err := pythonCommand(ctx)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, python, append(pythonArgs, script, "--versions")...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return "", fmt.Errorf("platform version check failed: %s", truncate(text, 500))
	}
	return text, nil
}

func distroScript() (string, error) {
	candidates := []string{
		os.Getenv("A365_DISTRO_SCRIPT"),
		"observability/export_run.py",
		filepath.Join("/app", "observability", "export_run.py"),
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, "observability", "export_run.py"),
			filepath.Join(dir, "..", "observability", "export_run.py"),
		)
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("microsoft-opentelemetry exporter script was not found")
}

func pythonCommand(ctx context.Context) (string, []string, error) {
	if python := strings.TrimSpace(os.Getenv("A365_PYTHON")); python != "" {
		return python, nil, nil
	}
	candidates := []struct {
		exe  string
		args []string
	}{
		{"py", []string{"-3.13"}},
		{"py", []string{"-3.12"}},
		{"python3.13", nil},
		{"python3.12", nil},
		{"python", nil},
		{"python3", nil},
	}
	for _, candidate := range candidates {
		path, err := exec.LookPath(candidate.exe)
		if err != nil {
			continue
		}
		checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		cmd := exec.CommandContext(checkCtx, path, append(candidate.args, "-c", "import microsoft.opentelemetry")...)
		err = cmd.Run()
		cancel()
		if err == nil {
			return path, candidate.args, nil
		}
	}
	return "", nil, fmt.Errorf("microsoft-opentelemetry is not installed for a supported Python. Install it with: py -3.13 -m pip install -r requirements.txt")
}
