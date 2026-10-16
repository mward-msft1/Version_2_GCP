package aigateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/openai/openai-go/v3/option"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/openaimodel"
)

// Model returns the ADK model used by the hosted agent. Calls go to the
// Azure AI Gateway Chat Completions API. The gateway requires Api-Key;
// the OpenAI client also sends Authorization, which this gateway ignores.
func Model(ctx context.Context, host, modelName, apiKey string) (model.LLM, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("AI_GATEWAY_API_KEY is not set")
	}
	if modelName == "" {
		modelName = DefaultModel
	}
	return openaimodel.NewModel(ctx, modelName, &openaimodel.ClientConfig{
		BaseURL: BaseURL(host),
		API:     openaimodel.APIChatCompletions,
		HTTPClient: &http.Client{
			Timeout: 90 * time.Second,
			Transport: apiKeyTransport{
				base: http.DefaultTransport,
				key:  apiKey,
			},
		},
		Options: []option.RequestOption{
			option.WithHeaderDel("Authorization"),
		},
	})
}

type apiKeyTransport struct {
	base http.RoundTripper
	key  string
}

func (t apiKeyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.Header.Del("Authorization")
	cloned.Header.Set("Api-Key", t.key)
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(cloned)
}

func BaseURL(host string) string {
	host = strings.TrimRight(strings.TrimSpace(host), "/")
	if host == "" {
		host = DefaultHost
	}
	const suffix = "/default/models/openai/v1"
	if strings.HasSuffix(strings.ToLower(host), suffix) {
		return host
	}
	return host + suffix
}
