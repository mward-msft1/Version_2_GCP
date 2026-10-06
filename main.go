package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/cmd/launcher"
	"google.golang.org/adk/v2/cmd/launcher/agentengine"
	"google.golang.org/adk/v2/cmd/launcher/full"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/mward-msft1/Version_2_GCP/internal/config"
	"github.com/mward-msft1/Version_2_GCP/internal/m365"
	"github.com/mward-msft1/Version_2_GCP/internal/purview"
	"github.com/mward-msft1/Version_2_GCP/internal/register"
	"github.com/mward-msft1/Version_2_GCP/internal/scenario"
	"github.com/mward-msft1/Version_2_GCP/internal/workiq"
)

func main() {
	cfg := config.Load()
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "register":
			result, err := register.Run(context.Background(), cfg)
			if err != nil {
				log.Fatal(err)
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(result); err != nil {
				log.Fatal(err)
			}
			fmt.Fprintln(os.Stderr, "Grant admin consent, then run: go run . login")
			fmt.Fprintln(os.Stderr, result.AdminConsentURL)
			return
		case "login":
			authClient, err := register.RuntimeAuth(cfg)
			if err != nil {
				log.Fatal(err)
			}
			if _, err := authClient.Token(context.Background()); err != nil {
				log.Fatal(err)
			}
			fmt.Println("Signed in. Token cached outside the repository.")
			return
		case "test":
			if err := runTest(cfg); err != nil {
				log.Fatal(err)
			}
			return
		case "validate":
			if err := validate(cfg); err != nil {
				log.Fatal(err)
			}
			return
		}
	}

	ctx := context.Background()
	modelClient, err := gemini.NewModel(ctx, envOr("GEMINI_MODEL", "gemini-flash-latest"), &genai.ClientConfig{
		Backend:  genai.BackendVertexAI,
		Project:  cfg.GCPProjectID,
		Location: cfg.Location,
	})
	if err != nil {
		log.Fatalf("create model: %v", err)
	}
	runner := &scenario.Runner{
		Config: cfg,
		Graph:  &m365.Client{},
		Purview: &purview.Client{
			AppID:   firstNonEmpty(cfg.RuntimeClientID, cfg.ClientID),
			AppName: cfg.AgentName,
		},
		WorkIQ: &workiq.Client{URL: cfg.WorkIQMCPURL},
	}
	dlpTool, err := functiontool.New(functiontool.Config{
		Name:        "run_dlp_test",
		Description: "Runs the DocSite DLP test: Purview inspection, then email to an approved internal user and the approved external recipient, plus a Teams chat and channel post. A Purview block is a successful test and stops that action.",
	}, func(ctx agent.Context, req scenario.Request) (scenario.Report, error) {
		if err := bindToken(cfg, runner); err != nil {
			return scenario.Report{}, err
		}
		return runner.Run(ctx, req)
	})
	if err != nil {
		log.Fatal(err)
	}

	instruction := cfg.Instructions + " You are Caldova GCP Agent Version 2. When the user asks you to run the test, call run_dlp_test. Use only the approved test users and the approved external recipient. If Purview blocks an action, report the block and do not retry that action. A block is a successful DLP test."
	a, err := llmagent.New(llmagent.Config{
		Name:        cfg.AgentID,
		Model:       modelClient,
		Description: cfg.Description,
		Instruction: instruction,
		Tools:       []tool.Tool{dlpTool},
		BeforeModelCallbacks: []llmagent.BeforeModelCallback{
			func(ctx agent.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
				if err := bindToken(cfg, runner); err != nil {
					return blocked(err.Error()), nil
				}
				decision, err := runner.Purview.Evaluate(ctx, "uploadText", requestText(req))
				if err != nil {
					return blocked(err.Error()), nil
				}
				if !decision.Allowed {
					return blocked(decision.Reason), nil
				}
				return nil, nil
			},
		},
		AfterModelCallbacks: []llmagent.AfterModelCallback{
			func(ctx agent.Context, resp *model.LLMResponse, respErr error) (*model.LLMResponse, error) {
				if respErr != nil || resp == nil || resp.Content == nil {
					return nil, nil
				}
				_, _ = runner.Purview.Evaluate(ctx, "uploadText", contentText(resp.Content))
				return nil, nil
			},
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	launchCfg := &launcher.Config{AgentLoader: agent.NewSingleLoader(a)}
	var l launcher.Launcher
	// Vertex starts the binary as `web -port 8080 agentengine`. That argument is
	// not a local interactive session, and the full launcher cannot parse it.
	if agentEngineArgs() || (cfg.AgentEngineID != "" && !interactiveArgs()) {
		l = agentengine.NewLauncher(cfg.AgentEngineID)
	} else {
		l = full.NewLauncher()
	}
	args := os.Args[1:]
	if err = l.Execute(ctx, launchCfg, args); err != nil {
		log.Fatalf("run failed: %v\n\n%s", err, l.CommandLineSyntax())
	}
}

func bindToken(cfg config.Config, runner *scenario.Runner) error {
	authClient, err := register.RuntimeAuth(cfg)
	if err != nil {
		return err
	}
	token, err := authClient.CachedToken()
	if err != nil {
		return fmt.Errorf("sign in with `go run . login` as CharlotteW or BrookeG before running the test")
	}
	runner.Graph.Token = token
	runner.Purview.Token = token
	runner.WorkIQ.Token = token
	if runner.Purview.UserEmail == "" {
		runner.Purview.UserEmail = cfg.ActingUser
	}
	return nil
}

func blocked(reason string) *model.LLMResponse {
	return &model.LLMResponse{
		Content: &genai.Content{
			Role:  "model",
			Parts: []*genai.Part{{Text: "Purview or sign-in blocked this interaction: " + reason}},
		},
		TurnComplete: true,
	}
}

func requestText(req *model.LLMRequest) string {
	if req == nil {
		return ""
	}
	var parts []string
	for _, content := range req.Contents {
		parts = append(parts, contentText(content))
	}
	return strings.Join(parts, "\n")
}

func contentText(content *genai.Content) string {
	if content == nil {
		return ""
	}
	var parts []string
	for _, part := range content.Parts {
		if part != nil && part.Text != "" {
			parts = append(parts, part.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func agentEngineArgs() bool {
	for _, arg := range os.Args[1:] {
		if arg == "agentengine" {
			return true
		}
	}
	return false
}

func interactiveArgs() bool {
	for _, arg := range os.Args[1:] {
		if arg == "console" || arg == "web" {
			return true
		}
	}
	return len(os.Args) == 1
}

func runTest(cfg config.Config) error {
	runner := &scenario.Runner{
		Config: cfg,
		Graph:  &m365.Client{},
		Purview: &purview.Client{
			AppID:   firstNonEmpty(cfg.RuntimeClientID, cfg.ClientID),
			AppName: cfg.AgentName,
		},
		WorkIQ: &workiq.Client{URL: cfg.WorkIQMCPURL},
	}
	if err := bindToken(cfg, runner); err != nil {
		return err
	}
	_, email, err := runner.Graph.Me(context.Background())
	if err != nil {
		return err
	}
	if !strings.HasSuffix(strings.ToLower(email), "@caldova56317036.onmicrosoft.com") {
		return fmt.Errorf("refusing to run the DLP test as %s; only a Caldova test user is allowed", email)
	}
	if !cfg.IsTestUser(email) {
		return fmt.Errorf("signed-in user %s is not CharlotteW or BrookeG", email)
	}
	cfg.ActingUser = email
	runner.Config.ActingUser = email
	only := ""
	if len(os.Args) > 2 {
		only = os.Args[2]
	}
	report, err := runner.Run(context.Background(), scenario.Request{ActingUser: email, Only: only})
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if encodeErr := enc.Encode(report); encodeErr != nil {
		return encodeErr
	}
	return err
}

func validate(cfg config.Config) error {
	fmt.Println("agent:", cfg.AgentName)
	fmt.Println("description:", cfg.Description)
	fmt.Println("tenant:", cfg.TenantID)
	fmt.Println("gcp project number:", cfg.GCPProjectNumber)
	fmt.Println("sharepoint:", cfg.SharePointHost+cfg.SharePointPath)
	fmt.Println("test users:", strings.Join(cfg.TestUsers, ", "))
	fmt.Println("external recipient:", cfg.ApprovedExternalRecipient)
	if cfg.ClientID == "" {
		fmt.Println("status: not registered yet. Run `go run . register` and complete the device-code sign-in as the Caldova admin.")
	} else {
		fmt.Println("client id:", cfg.ClientID)
		status, err := register.Verify(context.Background(), cfg)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(status); err != nil {
			return err
		}
		if !status.AdminConsentGranted {
			fmt.Println("admin consent still required:", fmt.Sprintf("https://login.microsoftonline.com/%s/adminconsent?client_id=%s", cfg.TenantID, cfg.ClientID))
		}
	}
	if cfg.WorkIQMCPURL == "" {
		fmt.Println("workiq: not configured; Graph is the execution path")
	} else {
		fmt.Println("workiq:", cfg.WorkIQMCPURL)
	}
	fmt.Println("local validation passed. Agent 365 objects are checked above from the Caldova admin token. Vertex deploy still needs gcloud signed in as the GCP account. This process will not use the corporate Azure CLI session and will not accept a pasted password.")
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
