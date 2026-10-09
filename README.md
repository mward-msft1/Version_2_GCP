# Caldova GCP Agent Version 2

Google ADK (Go) test agent for DLP, Purview DSPM, and Agent 365. It does not contain DLP rules. It calls Purview `protectionScopes/compute`, then `processContent`, so the published Caldova policy can log and block. Prompts are `uploadText` and responses are `downloadText` with one correlation ID, then recorded with `contentActivities`. File text is sent the same way before a send.

I am an agent built to test DLP, Incidents, and A365.

When prompted, you need to try and send a file to an approved internal and external recipient, post the file in a teams chat and channel.

## Boundaries

- Test initiators: `CharlotteW@Caldova56317036.onmicrosoft.com` and `BrookeG@Caldova56317036.onmicrosoft.com`. Charlotte is included on every email, Teams chat, and Teams channel function.
- Approved external recipient: `mward042@gmail.com`
- SharePoint library: `https://caldova56317036.sharepoint.com/sites/DocSite`
- Entra tenant: `b29b0240-e051-4989-8492-cafe1e25f54a`
- GCP project number: `833485904895`
- WorkIQ MCP defaults to `https://workiq.svc.cloud.microsoft/mcp`. DocSite listing resolves the site id, then lists `/drives/{driveId}/items/{rootId}/children`. WorkIQ denies the `/sites/{host}:{path}:/drive/root/children` alias. Download, mail, Teams chat, and Teams channel use `fetch_blob`, `do_action`, and `create_entity` for whichever of CharlotteW or BrookeG is signed in. Graph is the fallback. Set `WORKIQ_MCP_URL=off` to skip WorkIQ.
- Purview must allow an action before mail or Teams runs. A `restrictAccess` result is a successful DLP test and that action is not sent.
- Prompt and response text is captured only when the Know Your Data collection policy has ingestion enabled for `UploadText` and `DownloadText`.
- Passwords are never stored. Sign-in is device code only. Tokens stay in the user config directory, outside this repo.

There is no Agent 365 Go SDK. Registration uses the Graph v1.0 agent-identity APIs the A365 CLI uses: agent identity blueprint, blueprint principal, and agent identity. Agent 365 card registration remains the published `beta/copilot/agentRegistrations` API. Telemetry uses the Microsoft OpenTelemetry Distro `microsoft-opentelemetry` 1.4.0. The Python Agent 365 SDK packages are pinned at 1.0.0, the newest stable release. Purview `processContent` and `protectionScopes/compute` use Graph v1.0. Vertex deploy uses the Agent Engine v1 API. Python packages are `google-cloud-aiplatform` 2.3.0 and `google-adk` 2.10.0. Go libraries are ADK v2.5.0, genai v1.73.0, and `google.golang.org/api` v0.301.0.

## Run

```powershell
go test ./...
go run . validate
go run . register
go run . login
go run . console
```

`register` must be completed as `admin@caldova56317036.onmicrosoft.com`. The Agent 365 blueprint cannot use device code, so `login` uses the Caldova runtime public client written to `a365.generated.config.json`, then the WorkIQ public client. Sign in once as CharlotteW and once as BrookeG. Do not point `AZURE_CLIENT_ID` at the blueprint. Do not paste a password into the chat.

Say "run the DLP test" in the console.

## Publish

```powershell
gcloud auth login mward@msft365.info
.\scripts\deploy-vertex.ps1
```

Then in Microsoft 365 admin center, open Agents > Connected platforms, connect Google Vertex AI for project `833485904895` in `us-central1`, and sync. Grant the discovery service account `aiplatform.reasoningEngines.list` and `aiplatform.reasoningEngines.get`.

## DSPM

In Purview, onboard DSPM for AI and create a collection policy plus a DLP policy for the Entra application ID written to `a365.generated.config.json`. The agent sends that ID in `protectedAppMetadata.applicationLocation`.
