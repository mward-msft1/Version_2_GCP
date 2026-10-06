$ErrorActionPreference = "Stop"
Set-Location (Split-Path -Parent $PSScriptRoot)
go test ./...
go run . validate

$expected = "b29b0240-e051-4989-8492-cafe1e25f54a"
$corporate = "72f988bf-86f1-41af-91ab-2d7cd011db47"
$azd = az account show --query "{tenant:tenantId,user:user.name}" -o json | ConvertFrom-Json
if ($azd.tenant -eq $corporate) {
    Write-Host "Azure CLI default is corporate ($($azd.user)). It is not used for Agent 365 registration."
} elseif ($azd.tenant -eq $expected) {
    Write-Host "Azure CLI default is Caldova as $($azd.user)."
} else {
    Write-Host "Azure CLI default tenant $($azd.tenant) is ignored. Registration uses only Caldova $expected."
}
if (-not (Get-Command gcloud -ErrorAction SilentlyContinue)) {
    Write-Host "gcloud is not installed, so Vertex deployment was not validated."
}
