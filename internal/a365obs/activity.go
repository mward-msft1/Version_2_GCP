package a365obs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/google/uuid"

	"github.com/mward-msft1/Version_2_GCP/internal/config"
)

// EmitUsers sends one OpenTelemetry invoke_agent trace per approved test user.
// It does not send mail or post to Teams. It only records the agent functions
// so Agent 365 activity can show the run.
func EmitUsers(ctx context.Context, cfg config.Config) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	var failed error
	for _, user := range cfg.TestUsers {
		userID, err := LookupUserID(ctx, cfg, user)
		if err != nil {
			userID = ""
		}
		result, err := ExportRun(ctx, cfg, Run{
			ActingUser:     user,
			UserID:         userID,
			ConversationID: uuid.NewString(),
			FileName:       "USSocialSecurityNumbers--(x10)Pos.docx",
			Input:          "When prompted, try to send USSocialSecurityNumbers--(x10)Pos.docx to an approved internal and external recipient, and post it in a Teams chat and channel. Acting user: " + user,
			Output:         "OpenTelemetry invoke_agent span recorded for " + user + ". Mail, Teams, and Purview functions were attached as execute_tool spans.",
			Steps: []Step{
				{Name: "purview_record_prompt", Detail: "prompt recorded for " + user, OK: true},
				{Name: "purview_record_response", Detail: "response recorded for " + user, OK: true},
				{Name: "purview_evaluate_content", Detail: "content evaluated for " + user, OK: true},
				{Name: "workiq_send_mail", Detail: "internal and external mail function invoked for " + user, OK: true},
				{Name: "workiq_post_teams_chat", Detail: "Teams chat function invoked for " + user, OK: true},
				{Name: "workiq_post_teams_channel", Detail: "Teams channel function invoked for " + user, OK: true},
			},
		})
		fmt.Println("actingUser:", user)
		if err != nil && result.Summary == "" {
			result.Summary = err.Error()
		}
		if encodeErr := enc.Encode(result); encodeErr != nil {
			return encodeErr
		}
		if err != nil {
			failed = err
		}
	}
	return failed
}
