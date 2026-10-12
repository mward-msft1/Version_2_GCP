# How to integrate an S2S agent with Purview data security APIs

Customer setup template. Replace every `<placeholder>` before you share or run this guide. Do not paste passwords, client secrets, or file contents into chat, tickets, or source control.

This guide answers four questions for a service-to-service (S2S) agent that must call Microsoft Graph data security and governance APIs:

1. How `/security/dataSecurityAndGovernance` differs from `/users/{userId}/dataSecurityAndGovernance`.
2. Whether the agent needs its own user account, or can use the tenant endpoints instead.
3. How to provision a user account when one is required.
4. Which extra permissions the tenant endpoints need when a user account is not used.

Sources, checked 2026-10-12:

- [tenantDataSecurityAndGovernance: processContent (beta)](https://learn.microsoft.com/en-us/graph/api/tenantdatasecurityandgovernance-processcontent?view=graph-rest-beta)
- [tenantDataSecurityAndGovernance: processContentAsync (beta)](https://learn.microsoft.com/en-us/graph/api/tenantdatasecurityandgovernance-processcontentasync?view=graph-rest-beta)
- [tenantProtectionScopeContainer: compute (v1.0)](https://learn.microsoft.com/en-us/graph/api/tenantprotectionscopecontainer-compute?view=graph-rest-1.0)
- [userDataSecurityAndGovernance: processContent (v1.0)](https://learn.microsoft.com/en-us/graph/api/userdatasecurityandgovernance-processcontent?view=graph-rest-1.0)
- [userProtectionScopeContainer: compute (v1.0)](https://learn.microsoft.com/en-us/graph/api/userprotectionscopecontainer-compute?view=graph-rest-1.0)
- [Create contentActivity (v1.0)](https://learn.microsoft.com/en-us/graph/api/activitiescontainer-post-contentactivities?view=graph-rest-1.0)
- [Connect an existing agent to Agent 365](https://learn.microsoft.com/en-us/microsoft-agent-365/developer/get-started)

## Fill in before you start

| Item | Your value |
| --- | --- |
| Tenant ID | `<tenant-id>` |
| Agent display name | `<agent-display-name>` |
| Agent application or blueprint client ID | `<agent-client-id>` |
| Authentication mode | `S2S` or `delegated` |
| Acting user, if any | `<acting-user-upn>` |
| Acting user object ID, if any | `<acting-user-object-id>` |
| Protected application location value | `<policy-location-application-id>` |
| Tool location, for agent-to-tool checks | `<tool-location>` |

## Prerequisites

Complete these before either endpoint family will enforce policy:

1. Turn on Purview DLP for AI in `<tenant-id>`.
2. Complete DSPM for AI onboarding.
3. Confirm pay-as-you-go billing for the Purview AI workload.
4. Confirm at least one Microsoft 365 E7 or Microsoft Agent 365 license is assigned if you also export Agent 365 observability. A license that exists but is not assigned does not count.
5. Sign in as a Global Administrator, or as an Agent ID Developer with a Global Administrator available to grant consent.
6. Register the agent in Microsoft Entra. For Agent 365, a standard agent gets a blueprint and an agent identity. That registration does not create a mailbox.

Do not use a personal Microsoft account. These APIs support work or school accounts only.

## 1. How the two endpoint families differ

Use the path to decide the policy context. Do not treat them as aliases.

| | Tenant path | User path |
| --- | --- | --- |
| Resource | `tenantDataSecurityAndGovernance` | `userDataSecurityAndGovernance` |
| Base path | `/security/dataSecurityAndGovernance` | `/users/{userId}/dataSecurityAndGovernance` |
| Who the policy is evaluated for | The tenant, including agent-to-tool checks | One existing directory user |
| Signed-in user required | No, when you use an application token | No, if you pass `{userId}`. Yes, if you call `/me`. |
| Production API version | `protectionScopes/compute` is v1.0. `processContent` and `processContentAsync` are beta and are not supported for production. | `processContent`, `protectionScopes/compute`, and `contentActivities` are v1.0. |
| Content activity log | Not a relationship on the tenant resource | `activities/contentActivities` |
| S2S permission floor | `Content.Process.All` and `ProtectionScopes.Compute.All` | `Content.Process.User` and `ProtectionScopes.Compute.User`, or the `.All` permissions |

### Tenant calls

```http
POST https://graph.microsoft.com/v1.0/security/dataSecurityAndGovernance/protectionScopes/compute
POST https://graph.microsoft.com/beta/security/dataSecurityAndGovernance/processContent
POST https://graph.microsoft.com/beta/security/dataSecurityAndGovernance/processContentAsync
```

Use the synchronous tenant `processContent` call for an agent-to-tool check before the agent sends content to a tool:

- Set `evaluationScope.type` to `agent`.
- Identify the agent and the receiving tool in `protectedAppMetadata`.
- Call `protectionScopes/compute` first, cache the scope, and send its `ETag` as `If-None-Match`.
- Refresh the cache when the response `protectionScopeState` or `ETag` says the scope changed.

The URL has no user. An application token is valid.

`processContentAsync` is still posted to the tenant path, but each batch item includes a `userId`. That ID must already exist. The API does not create the user. Each text entry is limited to 2 MB, and one request can contain at most 64 content entries.

### User calls

```http
POST https://graph.microsoft.com/v1.0/users/{userId}/dataSecurityAndGovernance/protectionScopes/compute
POST https://graph.microsoft.com/v1.0/users/{userId}/dataSecurityAndGovernance/processContent
POST https://graph.microsoft.com/v1.0/users/{userId}/dataSecurityAndGovernance/activities/contentActivities
```

`/me/...` is not an S2S path. Microsoft Graph documents that `/me` requires a signed-in user and a delegated permission. Application permissions are not supported on `/me`.

The user path evaluates policy for that person. It is also the documented place to write a `contentActivity`, which returns the `userId` the activity was recorded for.

## 2. Does an S2S agent need a user account?

No, not for a tenant-scoped evaluation. A standard Agent 365 agent does not get its own user account. Call `/security/dataSecurityAndGovernance` with an application token when all of these are true:

- The agent runs in the background and has no signed-in person.
- The check is an agent-to-tool DLP decision.
- You can accept the beta status of tenant `processContent` until it is available in v1.0.
- You do not need a per-user `contentActivity` record.

You still need an existing directory user, addressed by object ID, when any of these are true:

- The policy decision must be that person's policy scope.
- You must write `contentActivities`.
- You call `processContentAsync` and supply `userId` on each batch item.
- Work IQ or another delegated Microsoft 365 tool must act as that person. Work IQ is skipped for a pure S2S observability setup.

That user does not have to be an account created for the agent. It is usually the person who started the interaction.

Create an account for the agent only when the agent itself must have a mailbox, Teams presence, or notification identity. In Agent 365 that is the AI teammate path, and it is limited to tenants in the Frontier preview program. A mailbox is not a substitute for the Graph application permissions below.

## 3. If you need a user, how do you provision one?

Pick one row. Do not do all three.

| Need | What to provision | What the agent uses |
| --- | --- | --- |
| Policy and audit for a person | Nothing new. Use `<acting-user-upn>`. | Application token plus `/users/{object-id}/...` |
| A dedicated mailbox to attribute background activity | A normal licensed work account, created by an administrator | Application token plus that account's object ID. Never its password. |
| The agent itself must send mail or appear in Teams | Agent 365 AI teammate, Frontier preview only | The user account created by that onboarding path |

### Use an existing person

1. Confirm `<acting-user-upn>` is a work or school account in `<tenant-id>`.
2. As an administrator, resolve the object ID. If the agent does this lookup itself, grant it application `User.Read.All` and admin consent.

```http
GET https://graph.microsoft.com/v1.0/users/<acting-user-upn>?$select=id,userPrincipalName,accountEnabled
```

3. Store `<acting-user-object-id>`. Pass that ID in the URL. Do not call `/me` from the S2S token.
4. Assign the licenses your scenario needs to that person. DLP for AI and DSPM for AI are tenant setup, not a license on the agent identity.
5. Sign the agent in with its application identity, not as the user.

### Create a dedicated service mailbox

Do this only when audit records must be attributed to a non-human mailbox and an existing person is not the right context.

1. In the Microsoft Entra admin center, create a work account such as `<agent-mailbox-upn>`.
2. Assign the mailbox and any required Microsoft 365 license.
3. Block interactive sign-in if no person should use the account.
4. Do not give the password, refresh token, or MFA device to the agent or to this guide.
5. Resolve the object ID with the same `GET /users/{upn}` call, then call `/users/{object-id}/dataSecurityAndGovernance/...` with the agent application token.

### Give the agent its own Agent 365 user

1. Confirm the tenant is in the Frontier preview program. If it is not, stop. This path is not available.
2. In the agent project, run the Agent 365 setup skill and follow the AI teammate route.
3. Ask the coding assistant to add messaging and notifications. That step creates the agent's user account, mailbox, and Teams presence.
4. Record the created user object ID as `<acting-user-object-id>`.
5. Continue to use S2S application permissions for `/security/...` or `/users/{id}/...`. The new mailbox does not grant those permissions.

## 4. If you do not use a user account, which permissions do you add?

Add these Microsoft Graph **application** permissions on `<agent-client-id>`, then grant tenant-wide admin consent. Delegated permissions do not authorize a client-credentials token.

| API | Least-privileged application permission | Higher permission |
| --- | --- | --- |
| `POST /security/dataSecurityAndGovernance/processContent` | `Content.Process.All` | None documented |
| `POST /security/dataSecurityAndGovernance/processContentAsync` | `Content.Process.All` | None documented |
| `POST /security/dataSecurityAndGovernance/protectionScopes/compute` | `ProtectionScopes.Compute.All` | None documented |

`Content.Process.User` and `ProtectionScopes.Compute.User` are not the permissions for the tenant path. They are the least-privileged application permissions for `/users/{userId}/...` only.

`ContentActivity.Write` does not appear on the tenant resource. Keep it only if you also write `/users/{userId}/dataSecurityAndGovernance/activities/contentActivities`.

### Grant the permissions

1. Open the agent app registration or Agent 365 blueprint for `<agent-client-id>` in Microsoft Entra.
2. Add the application permissions from the table. Do not add them as delegated permissions for an S2S agent.
3. Ask a Global Administrator to grant admin consent for `<tenant-id>`.
4. Acquire an application token for `https://graph.microsoft.com/.default`. Use a certificate, federated credential, or the Agent 365 client-secret FMI flow. Do not collect a user password.
5. Call `protectionScopes/compute` on the v1.0 tenant path and confirm HTTP 200 before you depend on a block or allow decision.
6. For an agent-to-tool check, call beta `processContent` only after you accept that beta APIs can change and are not supported for production.
7. Send `evaluationScope.type` as `agent`, and identify both the agent and the tool in `protectedAppMetadata`.
8. Treat a Purview block as success for the control. Do not continue the tool call after a block.

### Request shape for the tenant check

```http
POST https://graph.microsoft.com/beta/security/dataSecurityAndGovernance/processContent
Authorization: Bearer <application-access-token>
Content-Type: application/json
If-None-Match: <protection-scope-etag>

{
  "contentToProcess": {
    "contentEntries": [
      {
        "@odata.type": "microsoft.graph.processConversationMetadata",
        "identifier": "<content-id>",
        "name": "<content-name>",
        "content": {
          "@odata.type": "microsoft.graph.textContent",
          "data": "<text-to-evaluate>"
        }
      }
    ],
    "activityMetadata": { "activity": "uploadText" },
    "deviceMetadata": {
      "deviceType": "managed",
      "operatingSystemSpecifications": {
        "operatingSystemPlatform": "<os>",
        "operatingSystemVersion": "<os-version>"
      }
    },
    "evaluationScope": { "type": "agent" },
    "integratedAppMetadata": {
      "name": "<agent-display-name>",
      "version": "<agent-version>"
    },
    "protectedAppMetadata": {
      "name": "<tool-name>",
      "version": "<tool-version>",
      "applicationLocation": {
        "@odata.type": "microsoft.graph.policyLocationTool",
        "value": "<tool-location>"
      },
      "sourceLocation": {
        "@odata.type": "microsoft.graph.policyLocationApplication",
        "value": "<agent-client-id>"
      }
    }
  }
}
```

A `200 OK` with empty `policyActions` means no action was returned for that request. It does not by itself prove that DLP for AI is onboarded. Confirm DSPM onboarding and a known test policy before you treat an empty result as allow.

## Decision checklist

- [ ] DLP for AI, DSPM for AI, and pay-as-you-go billing are enabled in `<tenant-id>`.
- [ ] The agent is registered, and `<agent-client-id>` is the app that requests the Graph token.
- [ ] S2S calls use an application token. No call uses `/me`.
- [ ] Tenant evaluation uses `Content.Process.All` and `ProtectionScopes.Compute.All`.
- [ ] User evaluation, if used, targets `<acting-user-object-id>` and uses `Content.Process.User` or `Content.Process.All`.
- [ ] Content activity writes, if used, call the user path with `ContentActivity.Write`.
- [ ] An agent mailbox was created only for Frontier AI teammate messaging, not as a shortcut around Graph consent.
- [ ] Beta tenant `processContent` is either accepted as non-production or replaced with the v1.0 user path.
