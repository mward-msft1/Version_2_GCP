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

	"github.com/mward-msft1/Version_2_GCP/internal/a365obs"
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
			expected := ""
			if len(os.Args) > 2 {
				expected = os.Args[2]
				if !cfg.IsTestUser(expected) {
					log.Fatalf("%s is not CharlotteW or BrookeG", expected)
				}
			}
			var token string
			previous, hadPrevious := authClient.Snapshot()
			if expected != "" {
				token, err = authClient.DeviceToken(context.Background())
			} else {
				token, err = authClient.Token(context.Background())
			}
			if err != nil {
				log.Fatal(err)
			}
			_, email, err := (&m365.Client{Token: token}).Me(context.Background())
			if err != nil {
				log.Fatal(err)
			}
			if expected != "" && !strings.EqualFold(email, expected) {
				if hadPrevious {
					_ = authClient.Restore(previous)
				}
				log.Fatalf("signed in as %s, expected %s. The previous Graph token was restored.", email, expected)
			}
			if !cfg.IsTestUser(email) {
				log.Fatalf("signed-in user %s is not CharlotteW or BrookeG", email)
			}
			workiqAuth, err := workiq.Auth(cfg, email)
			if err != nil {
				log.Fatal(err)
			}
			if _, err := workiqAuth.Token(context.Background()); err != nil {
				log.Fatal(err)
			}
			fmt.Println("Signed in to Graph and WorkIQ as", email+". Token cached outside the repository.")
			fmt.Println("Run login again as the other test user so both CharlotteW and BrookeG have a WorkIQ token.")
			return
		case "test":
			if err := runTest(cfg); err != nil {
				log.Fatal(err)
			}
			return
		case "prompts":
			if err := runPrompts(cfg); err != nil {
				log.Fatal(err)
			}
			return
		case "validate":
			if err := validate(cfg); err != nil {
				log.Fatal(err)
			}
			return
		case "sync":
			if err := register.Sync(context.Background(), cfg); err != nil {
				log.Fatal(err)
			}
			return
		case "activity":
			if err := a365obs.EmitUsers(context.Background(), cfg); err != nil {
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
			AppID:           firstNonEmpty(cfg.RuntimeClientID, cfg.ClientID),
			AppName:         cfg.AgentName,
			BlueprintID:     cfg.BlueprintID,
			AgentIdentityID: cfg.AgentIdentityID,
		},
		WorkIQ: &workiq.Client{URL: cfg.WorkIQMCPURL},
	}
	dlpTool, err := functiontool.New(functiontool.Config{
		Name:        "run_dlp_test",
		Description: "Runs the DocSite DLP test: Purview inspection, then email to CharlotteW and BrookeG and the approved external recipient, plus a Teams chat with each of them and a Teams channel post. A Purview block is a successful test and stops that action.",
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
					emitTurn(ctx, cfg, runner, requestText(req), "Purview or sign-in blocked this interaction: "+err.Error())
					return blocked(err.Error()), nil
				}
				decision, err := runner.Purview.EvaluatePrompt(ctx, requestText(req))
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
				responseText := contentText(resp.Content)
				_, _ = runner.Purview.EvaluateResponse(ctx, responseText)
				emitTurn(ctx, cfg, runner, "Agent model turn", responseText)
				return nil, nil
			},
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("a365 telemetry config identity=%t blueprint=%t python=%s", cfg.AgentIdentityID != "", cfg.BlueprintID != "", os.Getenv("A365_PYTHON"))
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
	if runner.WorkIQ != nil {
		runner.WorkIQ.Token = ""
	}
	if runner.Purview.UserEmail == "" {
		runner.Purview.UserEmail = cfg.ActingUser
	}
	return nil
}

func emitTurn(ctx context.Context, cfg config.Config, runner *scenario.Runner, input, output string) {
	acting := cfg.ActingUser
	userID := ""
	if runner != nil && runner.Graph != nil && runner.Graph.Token != "" {
		if id, email, err := runner.Graph.Me(ctx); err == nil && email != "" {
			acting = email
			userID = id
		}
	}
	result, err := a365obs.ExportRun(ctx, cfg, a365obs.Run{
		ActingUser:     acting,
		UserID:         userID,
		ConversationID: "vertex-" + acting,
		Input:          input,
		Output:         output,
	})
	if err != nil {
		log.Printf("agent365 export failed: %s", err.Error())
		return
	}
	log.Printf("agent365 export status=%d %s", result.Status, result.Summary)
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
			AppID:           firstNonEmpty(cfg.RuntimeClientID, cfg.ClientID),
			AppName:         cfg.AgentName,
			BlueprintID:     cfg.BlueprintID,
			AgentIdentityID: cfg.AgentIdentityID,
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
	fileName := os.Getenv("DLP_TEST_FILE")
	if len(os.Args) > 2 {
		if strings.Contains(os.Args[2], ".") {
			fileName = os.Args[2]
		} else {
			only = os.Args[2]
		}
	}
	if len(os.Args) > 3 {
		fileName = os.Args[3]
	}
	report, err := runner.Run(context.Background(), scenario.Request{ActingUser: email, FileName: fileName, Only: only})
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if encodeErr := enc.Encode(report); encodeErr != nil {
		return encodeErr
	}
	return err
}

func runPrompts(cfg config.Config) error {
	runner := &scenario.Runner{
		Config: cfg,
		Graph:  &m365.Client{},
		Purview: &purview.Client{
			AppID:           firstNonEmpty(cfg.RuntimeClientID, cfg.ClientID),
			AppName:         cfg.AgentName,
			BlueprintID:     cfg.BlueprintID,
			AgentIdentityID: cfg.AgentIdentityID,
		},
	}
	if err := bindToken(cfg, runner); err != nil {
		return err
	}
	id, email, err := runner.Graph.Me(context.Background())
	if err != nil {
		return err
	}
	if !cfg.IsTestUser(email) {
		return fmt.Errorf("signed-in user %s is not CharlotteW or BrookeG", email)
	}
	runner.Purview.UserEmail = id
	snippet := ""
	fileName := "USSocialSecurityNumbers--(x10)Pos.docx"
	if driveID, files, listErr := runner.Graph.ListDocuments(context.Background(), cfg.SharePointHost, cfg.SharePointPath); listErr == nil {
		for _, item := range files {
			if strings.EqualFold(item.Name, fileName) {
				if file, prepErr := runner.Graph.PrepareFile(context.Background(), driveID, item); prepErr == nil {
					snippet = file.Snippet
				}
				break
			}
		}
	}
	pairs := promptPairs(email, fileName, snippet)
	type result struct {
		Pair     int    `json:"pair"`
		Activity string `json:"activity"`
		Allowed  bool   `json:"allowed"`
		Detail   string `json:"detail"`
	}
	var results []result
	for i, pair := range pairs {
		prompt, err := runner.Purview.EvaluatePrompt(context.Background(), pair[0])
		if err != nil {
			return err
		}
		results = append(results, result{Pair: i + 1, Activity: "uploadText", Allowed: prompt.Allowed, Detail: prompt.Reason})
		response, err := runner.Purview.EvaluateResponse(context.Background(), pair[1])
		if err != nil {
			return err
		}
		results = append(results, result{Pair: i + 1, Activity: "downloadText", Allowed: response.Allowed, Detail: response.Reason})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	fmt.Println("actingUser:", email)
	return enc.Encode(results)
}

func promptPairs(acting, fileName, snippet string) [][2]string {
	other := "BrookeG@Caldova56317036.onmicrosoft.com"
	if strings.Contains(strings.ToLower(acting), "brookeg") {
		other = "CharlotteW@Caldova56317036.onmicrosoft.com"
	}
	review := "I reviewed " + fileName + " for " + acting + ". It is a DocSite test file used to exercise DLP."
	if strings.TrimSpace(snippet) != "" {
		review += "\n" + snippet
	}
	return [][2]string{
		{
			"Summarize the DLP test plan for " + fileName + ". Acting user: " + acting,
			"The plan is to evaluate " + fileName + " in Purview, then try an internal send to " + other + " and an external send to mward042@gmail.com.",
		},
		{
			"What should you do before posting " + fileName + " in Teams? Acting user: " + acting,
			"I must send the prompt and the file text to Purview first. If Purview blocks it, I will not post it to the chat or channel.",
		},
		{
			"Draft the email you would send with " + fileName + ". Acting user: " + acting,
			"Subject: DLP test from Caldova GCP Agent Version 2. Body: this is a test of the published DLP policy. Recipients: " + other + " and mward042@gmail.com.",
		},
		{
			"Review " + fileName + " and record this prompt and response for Purview. Acting user: " + acting,
			review,
		},
		{
			"Who is allowed to start this DLP test, and which site holds the test files? Acting user: " + acting,
			"CharlotteW and BrookeG can start the test. The files are in the DocSite document library. External delivery is limited to mward042@gmail.com.",
		},
		{
			"If Purview blocks " + fileName + ", should you still email it or post it in Teams? Acting user: " + acting,
			"No. A Purview block is a successful DLP test. I will report the block and will not email or post " + fileName + ".",
		},
		{
			"Which channel and chat should receive " + fileName + "? Acting user: " + acting,
			"Post it in the one-to-one Teams chat with " + other + " and in the first joined team channel. Include the organization link, not a new copy outside those targets.",
		},
		{
			"Record this interaction for Agent 365 and Purview after reviewing " + fileName + ". Acting user: " + acting,
			"I recorded the prompt as uploadText and this response as downloadText for " + acting + ". Agent 365 receives the same turn through the OpenTelemetry distro.",
		},
	}
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
		fmt.Println("workiq: disabled; Graph is the execution path")
	} else {
		fmt.Println("workiq:", cfg.WorkIQMCPURL)
		fmt.Println("workiq tools: fetch, fetch_blob, do_action, create_entity")
	}
	platform, err := a365obs.VerifyPlatform(context.Background())
	if err != nil {
		return err
	}
	fmt.Println(platform)
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
