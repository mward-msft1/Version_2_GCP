package scenario

import (
	"context"
	"fmt"
	"html"
	"strings"

	"github.com/google/uuid"

	"github.com/mward-msft1/Version_2_GCP/internal/a365obs"
	"github.com/mward-msft1/Version_2_GCP/internal/config"
	"github.com/mward-msft1/Version_2_GCP/internal/m365"
	"github.com/mward-msft1/Version_2_GCP/internal/purview"
	"github.com/mward-msft1/Version_2_GCP/internal/workiq"
)

type Request struct {
	ActingUser string `json:"actingUser"`
	FileName   string `json:"fileName,omitempty"`
	Only       string `json:"only,omitempty"`
}

type Step struct {
	Name    string `json:"name"`
	Allowed bool   `json:"allowed"`
	Detail  string `json:"detail"`
}

type Report struct {
	ActingUser string `json:"actingUser"`
	FileName   string `json:"fileName"`
	Steps      []Step `json:"steps"`
}

type Runner struct {
	Config  config.Config
	Graph   *m365.Client
	Purview *purview.Client
	WorkIQ  *workiq.Client
}

func (r *Runner) Run(ctx context.Context, req Request) (report Report, err error) {
	var userID string
	defer r.exportActivity(ctx, &report, &userID)
	acting := strings.TrimSpace(req.ActingUser)
	if acting == "" {
		acting = r.Config.ActingUser
	}
	if !r.Config.IsTestUser(acting) {
		return Report{}, fmt.Errorf("acting user %s is not an approved test initiator", acting)
	}
	report = Report{ActingUser: acting}
	if id, email, meErr := r.Graph.Me(ctx); meErr == nil && id != "" {
		userID = id
		r.Purview.UserEmail = id
		if r.WorkIQ != nil && r.WorkIQ.Enabled() {
			if tok, tokenErr := workiq.Refresh(ctx, r.Config, email); tokenErr == nil {
				r.WorkIQ.Token = tok
			} else {
				// A Graph token is the wrong audience for WorkIQ. Leave it unset so calls fail closed to Graph.
				r.WorkIQ.Token = ""
			}
		}
	} else {
		r.Purview.UserEmail = "me"
	}

	fileName := strings.TrimSpace(req.FileName)
	if fileName == "" {
		fileName = "the DocSite document"
	}
	pairs := [][2]string{
		{
			"Send " + fileName + " to the approved internal user and " + r.Config.ApprovedExternalRecipient + ". Acting user: " + acting,
			"I will try to email " + fileName + " to the approved internal and external recipients.",
		},
		{
			"Post " + fileName + " in a Teams chat with the other test user and in a Teams channel. Acting user: " + acting,
			"I will post " + fileName + " in the Teams chat and channel for CharlotteW and BrookeG.",
		},
		{
			"Review " + fileName + " and record this prompt and response for Purview. Acting user: " + acting,
			"I reviewed " + fileName + " for " + acting + " and recorded the prompt and response.",
		},
	}
	blocked := false
	sequence := 1
	for i, pair := range pairs[:2] {
		correlation := uuid.NewString()
		if !r.recordPair(ctx, &report, fmt.Sprintf("prompt-%d", i+1), "uploadText", pair[0], correlation, sequence) {
			blocked = true
		}
		sequence++
		if !r.recordPair(ctx, &report, fmt.Sprintf("response-%d", i+1), "downloadText", pair[1], correlation, sequence) {
			blocked = true
		}
		sequence++
	}

	var files []m365.DriveItem
	var driveID string
	usedWorkIQ := false
	named := strings.TrimSpace(req.FileName)
	if r.WorkIQ != nil && r.WorkIQ.Enabled() {
		driveID, files, err = r.WorkIQ.ListDocuments(ctx, r.Config.SharePointHost, r.Config.SharePointPath)
		if err == nil {
			usedWorkIQ = true
			report.Steps = append(report.Steps, Step{Name: "workiq-list", Allowed: true, Detail: "listed through WorkIQ fetch"})
		} else {
			report.Steps = append(report.Steps, Step{Name: "workiq-list", Allowed: false, Detail: "WorkIQ MCP unavailable, using Graph: " + err.Error()})
			err = nil
		}
	}
	if named != "" && !hasFile(files, named) {
		id, item, findErr := r.Graph.FindDocument(ctx, r.Config.SharePointHost, r.Config.SharePointPath, named)
		if findErr == nil {
			driveID, files = id, []m365.DriveItem{item}
			report.Steps = append(report.Steps, Step{Name: "find-document", Allowed: true, Detail: item.Name})
		} else {
			report.Steps = append(report.Steps, Step{Name: "find-document", Allowed: false, Detail: findErr.Error()})
		}
	}
	if len(files) == 0 {
		driveID, files, err = r.Graph.ListDocuments(ctx, r.Config.SharePointHost, r.Config.SharePointPath)
	}
	if err != nil {
		report.Steps = append(report.Steps, Step{Name: "list-documents", Allowed: false, Detail: err.Error()})
		return report, nil
	}
	item, ok := pick(files, req.FileName)
	if named != "" && !strings.EqualFold(item.Name, named) {
		report.Steps = append(report.Steps, Step{Name: "list-documents", Allowed: false, Detail: "requested document was not found: " + named})
		return report, nil
	}
	if !ok {
		report.Steps = append(report.Steps, Step{Name: "list-documents", Allowed: false, Detail: "no files found in the DocSite document library"})
		return report, nil
	}
	report.FileName = item.Name
	report.Steps = append(report.Steps, Step{Name: "list-documents", Allowed: true, Detail: item.Name})

	if err := r.gate(ctx, &report, "download", "downloadFile", "Download DocSite file "+item.Name+" "+item.WebURL); err != nil {
		return report, nil
	}
	var file m365.FilePayload
	if usedWorkIQ {
		file, err = r.WorkIQ.Download(ctx, driveID, item)
		if err != nil {
			report.Steps = append(report.Steps, Step{Name: "workiq-download", Allowed: false, Detail: "WorkIQ fetch_blob unavailable, using Graph: " + err.Error()})
			file, err = r.Graph.PrepareFile(ctx, driveID, item)
		}
	} else {
		file, err = r.Graph.PrepareFile(ctx, driveID, item)
	}
	if err != nil {
		report.Steps = append(report.Steps, Step{Name: "download", Allowed: false, Detail: err.Error()})
		return report, nil
	}
	// Send the file text to Purview. The published policy decides whether it is sensitive.
	if err := r.gate(ctx, &report, "sensitive-content", "uploadText", file.Snippet); err != nil {
		blocked = true
	}
	reviewCorrelation := uuid.NewString()
	reviewPrompt := pairs[2][0] + "\n" + file.Snippet
	reviewResponse := pairs[2][1] + "\n" + file.Snippet
	if !r.recordPair(ctx, &report, "prompt-3", "uploadText", reviewPrompt, reviewCorrelation, sequence) {
		blocked = true
	}
	sequence++
	if !r.recordPair(ctx, &report, "response-3", "downloadText", reviewResponse, reviewCorrelation, sequence) {
		blocked = true
	}
	if blocked {
		report.Steps = append(report.Steps, Step{Name: "outbound", Allowed: false, Detail: "Purview blocked a prompt, response, or the document text, so the file was not sent"})
		return report, nil
	}

	internalUsers := r.Config.FunctionUsers(acting)
	subject := "DLP test from Caldova GCP Agent Version 2"
	body := "DLP test. Source: " + item.WebURL
	if file.Link != "" {
		body += "\nOrganization link: " + file.Link
	}
	if req.Only == "" || req.Only == "email-internal" {
		for _, internal := range internalUsers {
			r.tryMail(ctx, &report, "email-internal", internal, nil, subject, body, file)
		}
	}
	if req.Only == "" || req.Only == "email-external" {
		r.tryMail(ctx, &report, "email-external", r.Config.ApprovedExternalRecipient, internalUsers, subject, body, file)
	}
	if req.Only == "" || req.Only == "teams-chat" {
		for _, internal := range internalUsers {
			r.tryChat(ctx, &report, acting, internal, file)
		}
	}
	if req.Only == "" || req.Only == "teams-channel" {
		r.tryChannel(ctx, &report, internalUsers, file)
	}
	return report, nil
}

func (r *Runner) exportActivity(ctx context.Context, report *Report, userID *string) {
	if report == nil || report.ActingUser == "" {
		return
	}
	steps := make([]a365obs.Step, 0, len(report.Steps))
	for _, step := range report.Steps {
		steps = append(steps, a365obs.Step{Name: step.Name, Detail: step.Detail, OK: step.Allowed})
	}
	id := ""
	if userID != nil {
		id = *userID
	}
	result, err := a365obs.ExportRun(ctx, r.Config, a365obs.Run{
		ActingUser:     report.ActingUser,
		UserID:         id,
		ConversationID: uuid.NewString(),
		FileName:       report.FileName,
		Input:          "Run the DLP test for " + report.FileName + ". Acting user: " + report.ActingUser,
		Output:         "OpenTelemetry invoke_agent span recorded for " + report.ActingUser + ".",
		Steps:          steps,
	})
	if err != nil {
		report.Steps = append(report.Steps, Step{Name: "agent365-otel", Allowed: false, Detail: err.Error()})
		return
	}
	report.Steps = append(report.Steps, Step{Name: "agent365-otel", Allowed: true, Detail: result.Summary})
}

func (r *Runner) tryMail(ctx context.Context, report *Report, name, to string, cc []string, subject, body string, file m365.FilePayload) {
	if !r.Config.IsApprovedRecipient(to) {
		report.Steps = append(report.Steps, Step{Name: name, Allowed: false, Detail: "recipient is not on the approved list"})
		return
	}
	for _, extra := range cc {
		if !r.Config.IsApprovedRecipient(extra) {
			report.Steps = append(report.Steps, Step{Name: name, Allowed: false, Detail: "recipient is not on the approved list"})
			return
		}
	}
	text := "Send " + file.Item.Name + " by email to " + to
	if len(cc) > 0 {
		text += " and " + strings.Join(cc, ", ")
	}
	text += "\n" + file.Snippet
	if err := r.gate(ctx, report, name, "uploadFile", text); err != nil {
		return
	}
	if r.WorkIQ != nil && r.WorkIQ.Enabled() {
		if err := r.WorkIQ.SendMail(ctx, to, cc, subject, body, file); err == nil {
			report.Steps = append(report.Steps, Step{Name: name, Allowed: true, Detail: "sent through WorkIQ do_action /me/sendMail to " + to})
			return
		} else {
			report.Steps = append(report.Steps, Step{Name: name + "-workiq", Allowed: false, Detail: "WorkIQ send failed, using Graph: " + err.Error()})
		}
	}
	if err := r.Graph.SendMail(ctx, to, cc, subject, body, file); err != nil {
		report.Steps = append(report.Steps, Step{Name: name, Allowed: false, Detail: err.Error()})
		return
	}
	report.Steps = append(report.Steps, Step{Name: name, Allowed: true, Detail: "Graph sendMail accepted for " + to})
}

func (r *Runner) tryChat(ctx context.Context, report *Report, acting, other string, file m365.FilePayload) {
	text := "Post " + file.Item.Name + " to a Teams chat with " + other + "\n" + file.Snippet
	if err := r.gate(ctx, report, "teams-chat", "uploadFile", text); err != nil {
		return
	}
	if r.WorkIQ != nil && r.WorkIQ.Enabled() {
		if chatID, err := r.WorkIQ.PostChat(ctx, other, messageText(file, []string{other})); err == nil {
			report.Steps = append(report.Steps, Step{Name: "teams-chat", Allowed: true, Detail: "posted through WorkIQ create_entity to chat " + chatID})
			return
		} else {
			report.Steps = append(report.Steps, Step{Name: "teams-chat-workiq", Allowed: false, Detail: "WorkIQ chat failed, using Graph: " + err.Error()})
		}
	}
	actingID, _, err := r.Graph.Me(ctx)
	if err != nil {
		report.Steps = append(report.Steps, Step{Name: "teams-chat", Allowed: false, Detail: err.Error()})
		return
	}
	otherID := other
	if id, err := r.Graph.UserID(ctx, other); err == nil && id != "" {
		otherID = id
	}
	chatID, err := r.Graph.PostChat(ctx, actingID, otherID, messageHTML(file, []string{other}))
	if err != nil {
		report.Steps = append(report.Steps, Step{Name: "teams-chat", Allowed: false, Detail: err.Error()})
		return
	}
	report.Steps = append(report.Steps, Step{Name: "teams-chat", Allowed: true, Detail: "posted to chat " + chatID})
}

func (r *Runner) tryChannel(ctx context.Context, report *Report, users []string, file m365.FilePayload) {
	text := "Post " + file.Item.Name + " to a Teams channel for " + strings.Join(users, ", ") + "\n" + file.Snippet
	if err := r.gate(ctx, report, "teams-channel", "uploadFile", text); err != nil {
		return
	}
	if r.WorkIQ != nil && r.WorkIQ.Enabled() {
		if teamID, channelID, err := r.WorkIQ.PostChannel(ctx, r.Config.TeamsTeamID, r.Config.TeamsChannelID, messageText(file, users)); err == nil {
			report.Steps = append(report.Steps, Step{Name: "teams-channel", Allowed: true, Detail: "posted through WorkIQ create_entity to team " + teamID + " channel " + channelID})
			return
		} else {
			report.Steps = append(report.Steps, Step{Name: "teams-channel-workiq", Allowed: false, Detail: "WorkIQ channel failed, using Graph: " + err.Error()})
		}
	}
	teamID, channelID, err := r.Graph.PostChannel(ctx, r.Config.TeamsTeamID, r.Config.TeamsChannelID, messageHTML(file, users))
	if err != nil {
		report.Steps = append(report.Steps, Step{Name: "teams-channel", Allowed: false, Detail: err.Error()})
		return
	}
	report.Steps = append(report.Steps, Step{Name: "teams-channel", Allowed: true, Detail: "posted to team " + teamID + " channel " + channelID})
}

func (r *Runner) recordPair(ctx context.Context, report *Report, name, activity, text, correlation string, sequence int) bool {
	decision, err := r.Purview.Inspect(ctx, activity, text, correlation, sequence)
	if err != nil {
		report.Steps = append(report.Steps, Step{Name: "purview-" + name, Allowed: false, Detail: err.Error()})
		return false
	}
	report.Steps = append(report.Steps, Step{Name: "purview-" + name, Allowed: decision.Allowed, Detail: decision.Reason})
	return decision.Allowed
}

func (r *Runner) inspect(ctx context.Context, report *Report, name, activity, text, correlation string, sequence int) error {
	decision, err := r.Purview.Inspect(ctx, activity, text, correlation, sequence)
	if err != nil {
		report.Steps = append(report.Steps, Step{Name: "purview-" + name, Allowed: false, Detail: err.Error()})
		return err
	}
	report.Steps = append(report.Steps, Step{Name: "purview-" + name, Allowed: decision.Allowed, Detail: decision.Reason})
	if !decision.Allowed {
		return fmt.Errorf("%s", decision.Reason)
	}
	return nil
}

func (r *Runner) gate(ctx context.Context, report *Report, name, activity, text string) error {
	decision, err := r.Purview.Evaluate(ctx, activity, text)
	if err != nil {
		report.Steps = append(report.Steps, Step{Name: "purview-" + name, Allowed: false, Detail: err.Error()})
		return err
	}
	report.Steps = append(report.Steps, Step{Name: "purview-" + name, Allowed: decision.Allowed, Detail: decision.Reason})
	if !decision.Allowed {
		return fmt.Errorf("%s", decision.Reason)
	}
	return nil
}

func hasFile(files []m365.DriveItem, name string) bool {
	for _, file := range files {
		if strings.EqualFold(file.Name, name) {
			return true
		}
	}
	return false
}

func pick(files []m365.DriveItem, name string) (m365.DriveItem, bool) {
	if name != "" {
		for _, file := range files {
			if strings.EqualFold(file.Name, name) {
				return file, true
			}
		}
	}
	if len(files) == 0 {
		return m365.DriveItem{}, false
	}
	return files[0], true
}

func messageText(file m365.FilePayload, users []string) string {
	link := file.Link
	if link == "" {
		link = file.Item.WebURL
	}
	return "DLP test from Caldova GCP Agent Version 2 for " + strings.Join(users, ", ") + ". Document: " + file.Item.Name + " " + link
}

func messageHTML(file m365.FilePayload, users []string) string {
	link := file.Link
	if link == "" {
		link = file.Item.WebURL
	}
	return fmt.Sprintf("<p>DLP test from Caldova GCP Agent Version 2 for %s. Document: <a href=\"%s\">%s</a></p>", html.EscapeString(strings.Join(users, ", ")), html.EscapeString(link), html.EscapeString(file.Item.Name))
}
