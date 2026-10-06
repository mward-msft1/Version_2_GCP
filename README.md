# Caldova GCP Agent Version 2

Google ADK (Go) test agent for DLP, Purview DSPM, and Agent 365. It publishes to Vertex AI Agent Runtime and calls Microsoft Purview `processContent` before it reads DocSite or sends anything.

I am an agent built to test DLP, Incidents, and A365.

When prompted, you need to try and send a file to an approved internal and external recipient, post the file in a teams chat and channel.

## Boundaries

- Test initiators: `CharlotteW@Caldova56317036.onmicrosoft.com` and `BrookeG@Caldova56317036.onmicrosoft.com`
- Approved external recipient: `mward042@gmail.com`
- SharePoint library: `https://caldova56317036.sharepoint.com/sites/DocSite`
- Entra tenant: `b29b0240-e051-4989-8492-cafe1e25f54a`
- GCP project number: `833485904895`
- WorkIQ MCP is used when `WORKIQ_MCP_URL` is set. Otherwise the agent uses Microsoft Graph.
- Purview must allow an action before mail or Teams runs. A `restrictAccess` result is a successful DLP test and that action is not sent.
- Passwords are never stored. Sign-in is device code only. Tokens stay in the user config directory, outside this repo.

There is no Agent 365 Go SDK. Registration uses the same Microsoft Graph agent-identity APIs the A365 CLI and SDK use: agent identity blueprint, blueprint principal, and agent identity. Vertex agents then appear in Agent 365 through Connected platforms sync.

## Run

```powershell
go test ./...
go run . validate
go run . register
go run . login
go run . console
```

`register` must be completed as `admin@caldova56317036.onmicrosoft.com`. The Agent 365 blueprint cannot use device code, so `login` uses the Caldova runtime public client written to `a365.generated.config.json`. Sign in as CharlotteW or BrookeG. Do not point `AZURE_CLIENT_ID` at the blueprint.

Say "run the DLP test" in the console.

## Publish

```powershell
gcloud auth login mward@msft365.info
.\scripts\deploy-vertex.ps1
```

Then in Microsoft 365 admin center, open Agents > Connected platforms, connect Google Vertex AI for project `833485904895` in `us-central1`, and sync. Grant the discovery service account `aiplatform.reasoningEngines.list` and `aiplatform.reasoningEngines.get`.

## DSPM

In Purview, onboard DSPM for AI and create a collection policy plus a DLP policy for the Entra application ID written to `a365.generated.config.json`. The agent sends that ID in `protectedAppMetadata.applicationLocation`.
