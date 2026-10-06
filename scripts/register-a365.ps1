$ErrorActionPreference = "Stop"
Set-Location (Split-Path -Parent $PSScriptRoot)
Write-Host "Registering Caldova GCP Agent Version 2 with Agent 365."
Write-Host "Sign in with device code as admin@caldova56317036.onmicrosoft.com. Do not paste the password here."
go run . register
