#Requires -Version 5.1
<#
.SYNOPSIS
Install and verify CPU, CUDA, or Vulkan on the actual Windows host.
.DESCRIPTION
Creates a new installation and isolated test memories. CUDA/Vulkan require
positive actual GPU-offload evidence from smoke.py, in addition to inference,
persistence, and shutdown checks. Does not configure GPU passthrough or Hyper-V.
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Installer,
    [Parameter(Mandatory = $true)][ValidatePattern('^[a-fA-F0-9]{64}$')][string]$InstallerSHA256,
    [Parameter(Mandatory = $true)][ValidateSet('cpu', 'cuda', 'vulkan')][string]$Backend,
    [ValidateSet('qwen2.5-0.5b', 'qwen2.5-1.5b', 'qwen2.5-3b', 'gpt-oss-20b', 'gpt-oss-120b')][string]$Model = 'qwen2.5-1.5b',
    [ValidateRange(10, 3600)][int]$TurnTimeoutSeconds = 300,
    [string]$Python = 'python',
    [string]$ReportDirectory = (Join-Path (Get-Location) ('mini-fabrics-windows-' + [guid]::NewGuid().ToString('N')))
)

$ErrorActionPreference = 'Stop'
$Model = $Model.ToLowerInvariant()
if ($env:OS -ne 'Windows_NT') { throw 'Run this verification script on the actual Windows host.' }
. (Join-Path $PSScriptRoot 'Test-Common.ps1')
$artifact = Assert-FabricsArtifact -Path $Installer -ExpectedSHA256 $InstallerSHA256
$pythonPath = (Get-Command $Python -ErrorAction Stop).Source
$smoke = Join-Path (Split-Path -Parent $PSScriptRoot) 'smoke.py'
if (-not (Test-Path -LiteralPath $smoke -PathType Leaf)) { throw "Smoke runner not found at '$smoke'. Use this script from the source checkout." }
$report = New-FabricsEvidenceDirectory -Path $ReportDirectory
$installation = Join-Path $report 'installation'
$status = 'failed'
$stage = 'prerequisites'
$failure = $null
try {
    $code = Invoke-FabricsLoggedCommand -Executable $pythonPath -Arguments @('-c', 'import sys; sys.exit(sys.version_info < (3, 9))') -LogPath (Join-Path $report 'python.log')
    if ($code -ne 0) { throw 'Python 3.9 or newer is required for native test evidence.' }
    $stage = 'install'
    $code = Invoke-FabricsLoggedCommand -Executable $artifact -Arguments @('--prefix', $installation, '--model', $Model, '--backend', $Backend, '--non-interactive') -LogPath (Join-Path $report 'install.log')
    if ($code -ne 0) { throw "Installer failed (exit $code). Inspect install.log; unsupported GPU backends are not silently accepted." }
    $runtime = Join-Path $installation 'bin\fabrics.exe'
    $stage = 'doctor'
    $code = Invoke-FabricsLoggedCommand -Executable $runtime -Arguments @('--home', $installation, 'doctor') -LogPath (Join-Path $report 'doctor.log')
    if ($code -ne 0) { throw "Runtime doctor failed (exit $code)." }
    $stage = 'smoke'
    $arguments = @($smoke, '--runtime', $runtime, '--home', $installation, '--mode', 'balanced', '--expect-backend', $Backend, '--turn-timeout', "$TurnTimeoutSeconds", '--log-directory', (Join-Path $report 'logs'), '--report', (Join-Path $report 'smoke.json'))
    if ($Backend -ne 'cpu') { $arguments += '--require-gpu' }
    $code = Invoke-FabricsLoggedCommand -Executable $pythonPath -Arguments $arguments -LogPath (Join-Path $report 'smoke.log')
    if ($code -ne 0) { throw "Smoke verification failed (exit $code); inspect smoke.log and logs/smoke-*/runtime.log." }
    $stage = 'complete'
    $status = 'passed'
}
catch { $failure = $_.Exception.Message }
finally {
    [ordered]@{
        status = $status; last_stage = $stage; backend = $Backend; model = $Model
        turn_timeout_seconds = $TurnTimeoutSeconds
        installer_sha256 = $InstallerSHA256.ToLowerInvariant()
        gpu_evidence_required = ($Backend -ne 'cpu'); memory_isolation = 'new installation and temporary smoke memory database'
        report_directory = $report; failure = $failure
    } | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $report 'host-report.json') -Encoding UTF8
}
if ($failure) { throw $failure }
Write-Host "Windows $Backend verification passed. Evidence: $report"
