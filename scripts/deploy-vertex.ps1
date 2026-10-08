$ErrorActionPreference = "Stop"
Set-Location (Split-Path -Parent $PSScriptRoot)

$credPath = Join-Path $env:APPDATA "caldova-gcp-agent\blueprint-credential.json"
if (-not (Test-Path $credPath)) { throw "Blueprint credential is missing. Run activity once locally before deploying hosted telemetry." }
$blueprintSecret = [string](Get-Content -Raw $credPath | ConvertFrom-Json).secretText
if (-not $blueprintSecret) { throw "Blueprint credential has no secret. Hosted Agent 365 telemetry cannot start." }

$projectNumber = "833485904895"
$location = if ($env:GOOGLE_CLOUD_LOCATION) { $env:GOOGLE_CLOUD_LOCATION } else { "us-central1" }
if (-not (Get-Command gcloud -ErrorAction SilentlyContinue)) {
    throw "gcloud is not installed. Install the Google Cloud SDK, then run: gcloud auth login mward@msft365.info"
}

gcloud config set project $projectNumber
$projectId = gcloud projects describe $projectNumber --format="value(projectId)"
if (-not $projectId) { throw "Could not resolve a project ID for $projectNumber" }

gcloud services enable aiplatform.googleapis.com cloudresourcemanager.googleapis.com bigquery.googleapis.com cloudtrace.googleapis.com monitoring.googleapis.com --project $projectId
# v2.0.0 hardcodes golang:1.25. v2.4.0 reads go.mod (1.26.6) so the remote image can compile.
# adkgo on Windows embeds a backslash path in the generated Dockerfile and the
# remote build fails. Build the same archive the CLI would, with a Linux-safe
# Dockerfile, and create the Reasoning Engine directly.
$methodsFile = Join-Path ([System.IO.Path]::GetTempPath()) "caldova-class-methods.json"
$methodsGen = Join-Path (Get-Location) "tmp_listmethods.go"
@'
package main
import (
  "encoding/json"
  "os"
  "google.golang.org/adk/v2/server/agentengine"
)
func main() {
  methods, err := agentengine.ListClassMethods()
  if err != nil { panic(err) }
  if err := json.NewEncoder(os.Stdout).Encode(methods); err != nil { panic(err) }
}
'@ | Set-Content -Encoding ascii $methodsGen
try {
    go run .\tmp_listmethods.go | Set-Content -Encoding ascii $methodsFile
} finally {
    if (Test-Path $methodsGen) { Remove-Item -Force $methodsGen }
}

$stage = Join-Path ([System.IO.Path]::GetTempPath()) ("caldova-vertex-" + [guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $stage | Out-Null
try {
    # Agent Engine rejects source archives over 8 MB. Keep the local build output out.
    & robocopy . $stage /E /NFL /NDL /NJH /NJS /XD .git bin /XF *.exe | Out-Null
    if ($LASTEXITCODE -ge 8) { throw "Could not stage the deploy source" }
    @'
FROM golang:1.26.6 AS builder
ENV GOTOOLCHAIN=auto
WORKDIR /app
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o caldova-gcp-agent main.go

FROM python:3.12-slim-bookworm
WORKDIR /app
COPY requirements.txt .
# google-adk 2.10.0 meets the >=1.18.0 floor. Install it without deps so it
# does not downgrade the distro's opentelemetry-sdk 1.45 pin.
RUN pip install --no-cache-dir -r requirements.txt && pip install --no-cache-dir --no-deps google-adk==2.10.0
COPY --from=builder /app/caldova-gcp-agent /app/caldova-gcp-agent
COPY a365.generated.config.json /app/a365.generated.config.json
COPY observability /app/observability
ENV A365_PYTHON=/usr/local/bin/python
ENV ENABLE_OBSERVABILITY=true
ENV ENABLE_A365_OBSERVABILITY_EXPORTER=true
ENV A365_USE_S2S_ENDPOINT=true
EXPOSE 8080
CMD ["/app/caldova-gcp-agent", "web", "-host", "0.0.0.0", "-port", "8080", "agentengine"]
'@ | Set-Content -Encoding ascii (Join-Path $stage "Dockerfile")

    $archive = Join-Path ([System.IO.Path]::GetTempPath()) "caldova-vertex-archive.tgz"
    if (Test-Path $archive) { Remove-Item -Force $archive }
    tar -czf $archive -C $stage .
    $archiveBytes = [System.IO.File]::ReadAllBytes($archive)
    if ($archiveBytes.Length -gt 8388608) { throw "Source archive is $($archiveBytes.Length) bytes; Agent Engine limit is 8388608" }
    Write-Host "Source archive bytes: $($archiveBytes.Length)"

    $methods = Get-Content -Raw $methodsFile | ConvertFrom-Json
    $body = @{
        displayName = "Caldova GCP Agent Version 2"
        spec = @{
            sourceCodeSpec = @{
                inlineSource = @{ sourceArchive = [Convert]::ToBase64String($archiveBytes) }
                imageSpec = @{}
            }
            agentFramework = "google-adk"
            deploymentSpec = @{
                env = @(
                    @{ name = "GOOGLE_CLOUD_REGION"; value = $location }
                    @{ name = "NUM_WORKERS"; value = "1" }
                    @{ name = "GOOGLE_CLOUD_AGENT_ENGINE_ENABLE_TELEMETRY"; value = "true" }
                    @{ name = "OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT"; value = "true" }
                    @{ name = "WORKIQ_MCP_URL"; value = "https://workiq.svc.cloud.microsoft/mcp" }
                    @{ name = "A365_PYTHON"; value = "/usr/local/bin/python" }
                    @{ name = "ENABLE_OBSERVABILITY"; value = "true" }
                    @{ name = "ENABLE_A365_OBSERVABILITY_EXPORTER"; value = "true" }
                    @{ name = "A365_USE_S2S_ENDPOINT"; value = "true" }
                    @{ name = "A365_BLUEPRINT_SECRET"; value = $blueprintSecret }
                )
            }
            classMethods = $methods
        }
    }
    $jsonPath = Join-Path ([System.IO.Path]::GetTempPath()) "caldova-reasoning-engine.json"
    $body | ConvertTo-Json -Depth 30 -Compress | Set-Content -Encoding ascii $jsonPath

    $token = gcloud auth print-access-token
    $parent = "https://$location-aiplatform.googleapis.com/v1/projects/$projectId/locations/$location/reasoningEngines"
    $existingId = "5806617938186207232"
    $generatedEarly = Join-Path (Get-Location) "a365.generated.config.json"
    if (Test-Path $generatedEarly) {
        $early = Get-Content -Raw $generatedEarly | ConvertFrom-Json
        if ($early.reasoningEngine -match 'reasoningEngines/(\d+)$') { $existingId = $Matches[1] }
    }
    $uri = "$parent/${existingId}?updateMask=spec.sourceCodeSpec,spec.classMethods,spec.deploymentSpec"
    $updateBody = @{
        name = "projects/$projectId/locations/$location/reasoningEngines/$existingId"
        spec = $body.spec
    }
    $updateBody | ConvertTo-Json -Depth 30 -Compress | Set-Content -Encoding ascii $jsonPath
    try {
        $create = Invoke-RestMethod -Method Patch -Uri $uri -Headers @{ Authorization = "Bearer $token" } -ContentType "application/json" -InFile $jsonPath
    } catch {
        Write-Host "Update failed, creating a new Reasoning Engine: $($_.Exception.Message)"
        $body | ConvertTo-Json -Depth 30 -Compress | Set-Content -Encoding ascii $jsonPath
        $token = gcloud auth print-access-token
        $create = Invoke-RestMethod -Method Post -Uri $parent -Headers @{ Authorization = "Bearer $token" } -ContentType "application/json" -InFile $jsonPath
    }
    Write-Host "Operation: $($create.name)"
    $opUri = "https://$location-aiplatform.googleapis.com/v1/$($create.name)"
    $done = $false
    for ($i = 0; $i -lt 40 -and -not $done; $i++) {
        Start-Sleep -Seconds 30
        $token = gcloud auth print-access-token
        $op = Invoke-RestMethod -Method Get -Uri $opUri -Headers @{ Authorization = "Bearer $token" }
        $done = [bool]$op.done
        Write-Host ("Poll {0}: done={1}" -f ($i + 1), $done)
        if ($op.error) { throw ($op.error | ConvertTo-Json -Depth 8 -Compress) }
    }
    if (-not $done) { throw "Reasoning Engine create did not finish in 20 minutes" }
    $engine = $op.response.name
    Write-Host "Deployed Reasoning Engine: $engine"
    $generated = Join-Path (Get-Location) "a365.generated.config.json"
    if (Test-Path $generated) {
        $cfg = Get-Content -Raw $generated | ConvertFrom-Json
        $cfg | Add-Member -NotePropertyName reasoningEngine -NotePropertyValue $engine -Force
        $cfg | ConvertTo-Json -Depth 8 | Set-Content -Encoding utf8 $generated
    }
} finally {
    if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
    if ($jsonPath -and (Test-Path $jsonPath)) { Remove-Item -Force $jsonPath }
}

Write-Host "After deploy, connect Google Vertex AI in Microsoft 365 admin center > Agents > Connected platforms."
Write-Host "Use project $projectId ($projectNumber) and region $location, then sync agents."
