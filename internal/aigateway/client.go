package aigateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	DefaultHost  = "https://merry-meteor-4379.eastus2-01.ai.gateway.azure.com"
	DefaultModel = "gemini-2.5-flash"
)

type Client struct {
	Host   string
	APIKey string
	Model  string
	HTTP   *http.Client
}

type reply struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c Client) Chat(ctx context.Context, prompt string) (string, error) {
	host := strings.TrimRight(c.Host, "/")
	if host == "" {
		host = DefaultHost
	}
	model := c.Model
	if model == "" {
		model = DefaultModel
	}
	if c.APIKey == "" {
		return "", fmt.Errorf("AI_GATEWAY_API_KEY is not set")
	}
	body, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{{
			"role":    "user",
			"content": prompt,
		}},
		"max_tokens": 64,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/default/models/openai/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Api-Key", c.APIKey)
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 90 * time.Second}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var parsed reply
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("gateway HTTP %d: %s", resp.StatusCode, truncate(string(raw)))
	}
	if resp.StatusCode >= 300 || parsed.Error.Message != "" {
		code := parsed.Error.Code
		if code == "" {
			code = resp.Status
		}
		return "", fmt.Errorf("gateway %s: %s", code, parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("gateway returned no choices")
	}
	return parsed.Choices[0].Message.Content, nil
}

func truncate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 180 {
		return s[:180]
	}
	return s
}
