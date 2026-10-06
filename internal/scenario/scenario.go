package scenario

import (
	"context"
	"fmt"
	"html"
	"strings"

	"github.com/google/uuid"

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

func (r *Runner) Run(ctx context.Context, req Request) (Report, error) {
	acting := strings.TrimSpace(req.ActingUser)
	if acting == "" {
		acting = r.Config.ActingUser
	}
	if !r.Config.IsTestUser(acting) {
		return Report{}, fmt.Errorf("acting user %s is not an approved test initiator", acting)
	}
	report := Report{ActingUser: acting}
	if id, _, err := r.Graph.Me(ctx); err == nil && id != "" {
		r.Purview.UserEmail = id
	} else {
		r.Purview.UserEmail = "me"
	}

	correlation := uuid.NewString()
	prompt := r.Config.Instructions + " Acting user: " + acting
	if err := r.inspect(ctx, &report, "prompt", "uploadText", prompt, correlation, 1); err != nil {
		return report, nil
	}
	response := "I will try to send a DocSite file to CharlotteW and BrookeG, and to " + r.Config.ApprovedExternalRecipient + ", post the file in a Teams chat with CharlotteW, and post the file in a Teams channel for CharlotteW and BrookeG."
	if err := r.inspect(ctx, &report, "response", "downloadText", response, correlation, 2); err != nil {
		return report, nil
	}

	var files []m365.DriveItem
	var driveID string
	if r.WorkIQ != nil && r.WorkIQ.Enabled() {
		if _, err := r.WorkIQ.Call(ctx, "list_sharepoint_documents", map[string]any{
			"siteHost": r.Config.SharePointHost,
			"sitePath": r.Config.SharePointPath,
		}); err == nil {
			report.Steps = append(report.Steps, Step{Name: "workiq-list", Allowed: true, Detail: "WorkIQ MCP listed the library; Graph is still used to fetch the file for the DLP send test"})
		} else {
			report.Steps = append(report.Steps, Step{Name: "workiq-list", Allowed: false, Detail: "WorkIQ MCP unavailable, using Graph: " + err.Error()})
		}
	}
	var err error
	driveID, files, err = r.Graph.ListDocuments(ctx, r.Config.SharePointHost, r.Config.SharePointPath)
	if err != nil {
		report.Steps = append(report.Steps, Step{Name: "list-documents", Allowed: false, Detail: err.Error()})
		return report, nil
	}
	item, ok := pick(files, req.FileName)
	if !ok {
		report.Steps = append(report.Steps, Step{Name: "list-documents", Allowed: false, Detail: "no files found in the DocSite document library"})
		return report, nil
	}
	report.FileName = item.Name
	report.Steps = append(report.Steps, Step{Name: "list-documents", Allowed: true, Detail: item.Name})

	if err := r.gate(ctx, &report, "download", "downloadFile", "Download DocSite file "+item.Name+" "+item.WebURL); err != nil {
		return report, nil
	}
	file, err := r.Graph.PrepareFile(ctx, driveID, item)
	if err != nil {
		report.Steps = append(report.Steps, Step{Name: "download", Allowed: false, Detail: err.Error()})
		return report, nil
	}
	// Send the file text to Purview. The published policy decides whether it is sensitive.
	if err := r.gate(ctx, &report, "sensitive-content", "uploadText", file.Snippet); err != nil {
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
		if _, err := r.WorkIQ.Call(ctx, "send_mail", map[string]any{"to": to, "cc": cc, "subject": subject, "fileName": file.Item.Name}); err == nil {
			report.Steps = append(report.Steps, Step{Name: name, Allowed: true, Detail: "sent through WorkIQ MCP to " + to})
			return
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
		if _, err := r.WorkIQ.Call(ctx, "post_teams_chat", map[string]any{"to": other, "fileName": file.Item.Name, "link": file.Link}); err == nil {
			report.Steps = append(report.Steps, Step{Name: "teams-chat", Allowed: true, Detail: "posted through WorkIQ MCP"})
			return
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
		if _, err := r.WorkIQ.Call(ctx, "post_teams_channel", map[string]any{
			"teamId":    r.Config.TeamsTeamID,
			"channelId": r.Config.TeamsChannelID,
			"fileName":  file.Item.Name,
			"link":      file.Link,
			"users":     users,
		}); err == nil {
			report.Steps = append(report.Steps, Step{Name: "teams-channel", Allowed: true, Detail: "posted through WorkIQ MCP"})
			return
		}
	}
	teamID, channelID, err := r.Graph.PostChannel(ctx, r.Config.TeamsTeamID, r.Config.TeamsChannelID, messageHTML(file, users))
	if err != nil {
		report.Steps = append(report.Steps, Step{Name: "teams-channel", Allowed: false, Detail: err.Error()})
		return
	}
	report.Steps = append(report.Steps, Step{Name: "teams-channel", Allowed: true, Detail: "posted to team " + teamID + " channel " + channelID})
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

func messageHTML(file m365.FilePayload, users []string) string {
	link := file.Link
	if link == "" {
		link = file.Item.WebURL
	}
	return fmt.Sprintf("<p>DLP test from Caldova GCP Agent Version 2 for %s. Document: <a href=\"%s\">%s</a></p>", html.EscapeString(strings.Join(users, ", ")), html.EscapeString(link), html.EscapeString(file.Item.Name))
}
