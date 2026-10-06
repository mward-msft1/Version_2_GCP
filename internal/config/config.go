package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

const (
	AgentDisplayName = "Caldova GCP Agent Version 2"
	AgentID          = "caldova_gcp_agent_v2"
	Description      = "I am an agent built to test DLP, Incidents, and A365."
	UserInstruction  = "When prompted, you need to try and send a file to an approved internal and external recipient, post the file in a teams chat and channel."
	TenantID         = "b29b0240-e051-4989-8492-cafe1e25f54a"
	GCPProjectNumber = "833485904895"
	SharePointHost   = "caldova56317036.sharepoint.com"
	SharePointPath   = "/sites/DocSite"
	ExternalEmail    = "mward042@gmail.com"
	GraphAppID       = "00000003-0000-0000-c000-000000000000"
	// Microsoft Graph Command Line Tools, a first-party public client used only
	// to bootstrap Agent 365 registration. No secret is stored.
	BootstrapClientID = "14d82eec-204b-4c2f-b7e8-296a70dab67e"
)

var TestUsers = []string{
	"CharlotteW@Caldova56317036.onmicrosoft.com",
	"BrookeG@Caldova56317036.onmicrosoft.com",
}

type File struct {
	AgentName                 string   `json:"agentName"`
	AgentID                   string   `json:"agentId"`
	Description               string   `json:"description"`
	Instructions              string   `json:"instructions"`
	TenantID                  string   `json:"tenantId"`
	GCPProjectNumber          string   `json:"gcpProjectNumber"`
	GCPLocation               string   `json:"gcpLocation"`
	SharePointHost            string   `json:"sharePointHost"`
	SharePointPath            string   `json:"sharePointPath"`
	TestUsers                 []string `json:"testUsers"`
	ApprovedExternalRecipient string   `json:"approvedExternalRecipient"`
}

type Config struct {
	File
	GCPProjectID    string
	ClientID        string
	RuntimeClientID string
	WorkIQMCPURL   string
	TeamsTeamID    string
	TeamsChannelID string
	ActingUser     string
	Location       string
	AgentEngineID  string
	GeneratedPath  string
}

func Load() Config {
	cfg := Config{
		File: File{
			AgentName:                 AgentDisplayName,
			AgentID:                   AgentID,
			Description:               Description,
			Instructions:              UserInstruction,
			TenantID:                  TenantID,
			GCPProjectNumber:          GCPProjectNumber,
			GCPLocation:               "us-central1",
			SharePointHost:            SharePointHost,
			SharePointPath:            SharePointPath,
			TestUsers:                 append([]string(nil), TestUsers...),
			ApprovedExternalRecipient: ExternalEmail,
		},
		Location:      "us-central1",
		GeneratedPath: "a365.generated.config.json",
	}
	if b, err := os.ReadFile("a365.config.json"); err == nil {
		_ = json.Unmarshal(b, &cfg.File)
	}
	if v := os.Getenv("ENTRA_TENANT_ID"); v != "" {
		cfg.TenantID = v
	}
	if v := os.Getenv("GCP_PROJECT_NUMBER"); v != "" {
		cfg.GCPProjectNumber = v
	}
	cfg.GCPProjectID = firstEnv("GOOGLE_CLOUD_PROJECT", "GCP_PROJECT_ID")
	if cfg.GCPProjectID == "" {
		cfg.GCPProjectID = cfg.GCPProjectNumber
	}
	if v := firstEnv("GOOGLE_CLOUD_LOCATION", "GCP_LOCATION"); v != "" {
		cfg.Location = v
		cfg.GCPLocation = v
	}
	cfg.ClientID = os.Getenv("AZURE_CLIENT_ID")
	cfg.WorkIQMCPURL = os.Getenv("WORKIQ_MCP_URL")
	cfg.TeamsTeamID = os.Getenv("TEAMS_TEAM_ID")
	cfg.TeamsChannelID = os.Getenv("TEAMS_CHANNEL_ID")
	cfg.ActingUser = os.Getenv("ACTING_USER")
	if cfg.ActingUser == "" && len(cfg.TestUsers) > 0 {
		cfg.ActingUser = cfg.TestUsers[0]
	}
	cfg.AgentEngineID = os.Getenv("GOOGLE_CLOUD_AGENT_ENGINE_ID")
	if gen, err := os.ReadFile(cfg.GeneratedPath); err == nil {
		var extra struct {
			ClientID        string `json:"clientId"`
			RuntimeClientID string `json:"runtimeClientId"`
			AgentEngineID   string `json:"agentEngineId"`
			GCPProjectID    string `json:"gcpProjectId"`
		}
		if json.Unmarshal(gen, &extra) == nil {
			if cfg.ClientID == "" {
				cfg.ClientID = extra.ClientID
			}
			cfg.RuntimeClientID = extra.RuntimeClientID
			if cfg.AgentEngineID == "" {
				cfg.AgentEngineID = extra.AgentEngineID
			}
			if extra.GCPProjectID != "" && os.Getenv("GOOGLE_CLOUD_PROJECT") == "" && extra.GCPProjectID != cfg.GCPProjectNumber {
				cfg.GCPProjectID = extra.GCPProjectID
			}
		}
	}
	return cfg
}

func (c Config) ApprovedRecipients() []string {
	out := append([]string{}, c.TestUsers...)
	if c.ApprovedExternalRecipient != "" {
		out = append(out, c.ApprovedExternalRecipient)
	}
	return out
}

func (c Config) IsTestUser(email string) bool {
	return containsFold(c.TestUsers, email)
}

func (c Config) IsApprovedRecipient(email string) bool {
	return containsFold(c.ApprovedRecipients(), email)
}

func (c Config) OtherTestUser(email string) string {
	for _, user := range c.TestUsers {
		if !strings.EqualFold(user, email) {
			return user
		}
	}
	return ""
}

func (c Config) TokenPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "caldova-gcp-agent")
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(path, "tokens.json"), nil
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return ""
}
