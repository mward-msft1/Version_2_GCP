# Adapted from microsoft/purview-api-samples
# DLPforCustomAIApps/Create-DlpPolicyForCustomAIApps.ps1
# Scoped to the Caldova runtime app that processContent reports.
$ErrorActionPreference = "Stop"

$DlpPolicyName = "Agent DLP Policy V2"
$DlpRuleName   = "Agent DLP Policy V2 Rule"
$PolicyMode = "Enable"
$RestrictAction = "Block"
$Applications = @(
    @{
        AppId   = "04bc495e-38e5-420f-8b0c-1bb52773305a"
        AppName = "Caldova GCP Agent Version 2"
    }
)
$AlertRecipients     = @("admin@caldova56317036.onmicrosoft.com", "CharlotteW@Caldova56317036.onmicrosoft.com")
$IncidentRecipients  = @("admin@caldova56317036.onmicrosoft.com", "CharlotteW@Caldova56317036.onmicrosoft.com")
$NotifyRecipients    = @("admin@caldova56317036.onmicrosoft.com", "CharlotteW@Caldova56317036.onmicrosoft.com")
$ReportSeverityLevel = "High"
$SensitiveTypes = @(
    @{ Name = "Credit Card Number"; minCount = "1" },
    @{ Name = "U.S. Social Security Number (SSN)"; minCount = "1" },
    @{ Name = "U.S. Bank Account Number"; minCount = "1" },
    @{ Name = "U.S. Individual Taxpayer Identification Number (ITIN)"; minCount = "1" },
    @{ Name = "U.S. / U.K. Passport Number"; minCount = "1" },
    @{ Name = "International Banking Account Number (IBAN)"; minCount = "1" },
    @{ Name = "ABA Routing Number"; minCount = "1" },
    @{ Name = "U.S. Driver's License Number"; minCount = "1" }
)

if (-not (Get-Module -ListAvailable -Name ExchangeOnlineManagement)) {
    throw "ExchangeOnlineManagement module is not installed."
}
Import-Module ExchangeOnlineManagement -ErrorAction Stop

$tokenPath = Join-Path $env:TEMP "caldova-ipps-token.json"
if (-not (Test-Path $tokenPath)) { throw "Caldova compliance sign-in token is missing." }
$token = (Get-Content -Raw $tokenPath | ConvertFrom-Json).access_token
$part = $token.Split('.')[1]
$pad = $part.Length % 4
if ($pad) { $part += "=" * (4 - $pad) }
$claims = [System.Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($part.Replace("-", "+").Replace("_", "/"))) | ConvertFrom-Json
if ($claims.tid -ne "b29b0240-e051-4989-8492-cafe1e25f54a") {
    throw "Refusing to create the policy outside the Caldova tenant."
}
if ($claims.upn -notlike "*@caldova56317036.onmicrosoft.com") {
    throw "Refusing to create the policy with a non-Caldova admin."
}

Write-Host "Connecting to Security and Compliance PowerShell as $($claims.upn)..."
Connect-IPPSSession -AccessToken $token -Organization "caldova56317036.onmicrosoft.com" -ShowBanner:$false

$aba = Get-DlpSensitiveInformationType -Identity "ABA Routing Number" -ErrorAction SilentlyContinue
if (-not $aba) {
    Write-Host "ABA Routing Number sensitive type was not found. Continuing with the sample types only."
    $SensitiveTypes = $SensitiveTypes | Where-Object { $_.Name -ne "ABA Routing Number" }
}

$LocationsObject = foreach ($app in $Applications) {
    @{
        Workload            = "Applications"
        Location            = $app.AppId
        LocationDisplayName = $app.AppName
        LocationSource      = "Entra"
        LocationType        = "Individual"
        Inclusions          = @(@{ Type = "Tenant"; Identity = "All" })
    }
}
$LocationsJson = $LocationsObject | ConvertTo-Json -Depth 6 -Compress
if ($LocationsJson.TrimStart().StartsWith("{")) { $LocationsJson = "[$LocationsJson]" }

Write-Host "Ensuring DLP policy '$DlpPolicyName' exists..."
$existingPolicy = Get-DlpCompliancePolicy -Identity $DlpPolicyName -ErrorAction SilentlyContinue
$policyParams = @{
    Comment            = "Blocks common sensitive information types for Caldova GCP Agent Version 2 across Exchange, SharePoint, OneDrive, and Teams."
    Locations          = $LocationsJson
    EnforcementPlanes  = @("Application")
    ExchangeLocation   = "All"
    SharePointLocation = "All"
    OneDriveLocation   = "All"
    TeamsLocation      = "All"
    Mode               = $PolicyMode
}
if (-not $existingPolicy) {
    New-DlpCompliancePolicy -Name $DlpPolicyName @policyParams | Out-Null
    Write-Host "DLP policy created."
}
else {
    Set-DlpCompliancePolicy -Identity $DlpPolicyName -Mode $PolicyMode -Comment $policyParams.Comment | Out-Null
    # RestrictAccess is rejected when Teams is on the same policy. Teams is covered by the companion policy below.
    Set-DlpCompliancePolicy -Identity $DlpPolicyName -RemoveTeamsLocation "All" -ErrorAction SilentlyContinue | Out-Null
    Write-Host "DLP policy already exists; enforcement mode reapplied."
}

Write-Host "Ensuring DLP rule '$DlpRuleName' exists..."
$existingRule = Get-DlpComplianceRule -Identity $DlpRuleName -ErrorAction SilentlyContinue
$ruleParams = @{
    ContentContainsSensitiveInformation = $SensitiveTypes
    GenerateAlert                       = $AlertRecipients
    GenerateIncidentReport              = $IncidentRecipients
    IncidentReportContent               = @("Default", "Detections", "DetectionDetails", "MatchedItem", "RulesMatched", "Service", "Severity", "Title")
    NotifyUser                          = $NotifyRecipients
    ReportSeverityLevel                 = $ReportSeverityLevel
    BlockAccess                         = $true
    RestrictAccess                      = @(
        @{ Setting = "UploadText"; Value = $RestrictAction },
        @{ Setting = "DownloadText"; Value = $RestrictAction }
    )
    Comment                             = "Blocks prompts and responses containing the listed sensitive information types."
}
if (-not $existingRule) {
    New-DlpComplianceRule -Name $DlpRuleName -Policy $DlpPolicyName @ruleParams | Out-Null
    Write-Host "DLP rule created."
}
else {
    try {
        Set-DlpComplianceRule -Identity $DlpRuleName @ruleParams -ErrorAction Stop | Out-Null
        Write-Host "DLP rule updated."
    }
    catch {
        Write-Host "DLP rule update deferred: $($_.Exception.Message)"
    }
}

# Enable is the published enforcement mode. Re-apply it so a new rule is not left in a draft policy.
Set-DlpCompliancePolicy -Identity $DlpPolicyName -Mode Enable | Out-Null

# Purview rejects RestrictAccess on a policy that includes Teams. Publish a companion policy so Teams is still in scope.
$TeamsPolicyName = "Agent DLP Policy V2 Teams"
$TeamsRuleName = "Agent DLP Policy V2 Teams Rule"
Write-Host "Ensuring Teams companion policy '$TeamsPolicyName' exists..."
$existingTeamsPolicy = Get-DlpCompliancePolicy -Identity $TeamsPolicyName -ErrorAction SilentlyContinue
if (-not $existingTeamsPolicy) {
    New-DlpCompliancePolicy -Name $TeamsPolicyName -Mode Enable -TeamsLocation "All" -Comment "Teams scope for Agent DLP Policy V2. Separate because RestrictAccess cannot be combined with Teams." | Out-Null
    Write-Host "Teams companion policy created."
}
else {
    Set-DlpCompliancePolicy -Identity $TeamsPolicyName -Mode Enable | Out-Null
    Write-Host "Teams companion policy already exists."
}
$existingTeamsRule = Get-DlpComplianceRule -Identity $TeamsRuleName -ErrorAction SilentlyContinue
$teamsRuleParams = @{
    ContentContainsSensitiveInformation = $SensitiveTypes
    GenerateAlert                       = $AlertRecipients
    GenerateIncidentReport              = $IncidentRecipients
    IncidentReportContent               = @("Default", "Detections", "DetectionDetails", "MatchedItem", "RulesMatched", "Service", "Severity", "Title")
    NotifyUser                          = $NotifyRecipients
    ReportSeverityLevel                 = $ReportSeverityLevel
    BlockAccess                         = $true
    StopPolicyProcessing                = $true
    Comment                             = "Blocks Teams messages that contain the listed sensitive information types."
}
if (-not $existingTeamsRule) {
    New-DlpComplianceRule -Name $TeamsRuleName -Policy $TeamsPolicyName @teamsRuleParams | Out-Null
    Write-Host "Teams companion rule created."
}
else {
    try {
        Set-DlpComplianceRule -Identity $TeamsRuleName @teamsRuleParams -ErrorAction Stop | Out-Null
        Write-Host "Teams companion rule updated."
    }
    catch {
        Write-Host "Teams companion rule update deferred: $($_.Exception.Message)"
    }
}

# Existing rules can be locked while distribution is pending. A dedicated rule still enforces the driver's license SIT.
$DriverLicenseType = @($SensitiveTypes | Where-Object { $_.Name -eq "U.S. Driver's License Number" })
foreach ($dlRule in @(
    @{ Name = "Agent DLP Policy V2 DL Rule"; Policy = $DlpPolicyName; Restrict = $true },
    @{ Name = "Agent DLP Policy V2 Teams DL Rule"; Policy = $TeamsPolicyName; Restrict = $false }
)) {
    if (Get-DlpComplianceRule -Identity $dlRule.Name -ErrorAction SilentlyContinue) {
        Write-Host "$($dlRule.Name) already exists."
        continue
    }
    $dlParams = @{
        ContentContainsSensitiveInformation = $DriverLicenseType
        ReportSeverityLevel                 = $ReportSeverityLevel
        BlockAccess                         = $true
        Comment                             = "Adds U.S. Driver's License Number while the parent rule is locked."
    }
    if ($dlRule.Restrict) {
        $dlParams.RestrictAccess = @(
            @{ Setting = "UploadText"; Value = $RestrictAction },
            @{ Setting = "DownloadText"; Value = $RestrictAction }
        )
        $dlParams.GenerateAlert = $AlertRecipients
        $dlParams.GenerateIncidentReport = $IncidentRecipients
        $dlParams.NotifyUser = $NotifyRecipients
    }
    New-DlpComplianceRule -Name $dlRule.Name -Policy $dlRule.Policy @dlParams | Out-Null
    Write-Host "$($dlRule.Name) created."
}
Set-DlpCompliancePolicy -Identity $TeamsPolicyName -Mode Enable | Out-Null

$CollectionName = "DSPM for AI - Collection policy for enterprise AI apps"
$CollectionConfig = '{"Activities":["UploadText","DownloadText"],"EnforcementPlanes":["Application"],"SensitiveTypeIds":["All"],"IsIngestionEnabled":true}'
$CollectionLocation = @{
    Workload            = "Applications"
    Location            = $Applications[0].AppId
    LocationDisplayName = $Applications[0].AppName
    LocationSource      = "Entra"
    LocationType        = "Individual"
    Inclusions          = @(@{ Type = "Tenant"; Identity = "All" })
}
$CollectionJson = $CollectionLocation | ConvertTo-Json -Depth 6 -Compress
if ($CollectionJson.TrimStart().StartsWith("{")) { $CollectionJson = "[$CollectionJson]" }
Write-Host "Ensuring collection policy '$CollectionName' stores prompts and responses..."
$existingCollection = Get-FeatureConfiguration -FeatureScenario KnowYourData -Identity $CollectionName -ErrorAction SilentlyContinue
if (-not $existingCollection) {
    New-FeatureConfiguration -FeatureScenario KnowYourData -Name $CollectionName -Mode Enable -ScenarioConfig $CollectionConfig -Locations $CollectionJson | Out-Null
    Write-Host "Collection policy created with ingestion enabled."
}
else {
    Set-FeatureConfiguration -Identity $CollectionName -Mode Enable -ScenarioConfig $CollectionConfig -Locations $CollectionJson | Out-Null
    Write-Host "Collection policy updated with ingestion enabled."
}

Write-Host "Verification:"
Get-DlpCompliancePolicy -Identity $DlpPolicyName | Format-List Name, Mode, Enabled, DistributionStatus, EnforcementPlanes, ExchangeLocation, SharePointLocation, OneDriveLocation, TeamsLocation, Locations
Get-DlpComplianceRule -Identity $DlpRuleName | Format-List Name, Policy, Mode, ReportSeverityLevel, RestrictAccess
Get-DlpCompliancePolicy -Identity $TeamsPolicyName | Format-List Name, Mode, Enabled, DistributionStatus, TeamsLocation
Get-DlpComplianceRule -Identity $TeamsRuleName | Format-List Name, Policy, Mode, ReportSeverityLevel
