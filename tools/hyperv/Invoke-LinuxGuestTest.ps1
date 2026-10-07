#Requires -Version 5.1
<#
.SYNOPSIS
Install and smoke-test a verified native Linux installer over existing SSH keys.
.DESCRIPTION
Uses strict known-host verification and noninteractive key authentication.
Installs under a unique /tmp directory inside the guest, preserving every prior
installation and its memory. Retrieves evidence even when the guest test fails.
No passwords, guest package installation, VM creation, or automatic cleanup.
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][ValidatePattern('^[A-Za-z0-9][A-Za-z0-9.-]*$')][string]$HostName,
    [Parameter(Mandatory = $true)][ValidatePattern('^[a-z_][a-z0-9_-]{0,31}$')][string]$UserName,
    [Parameter(Mandatory = $true)][string]$Installer,
    [Parameter(Mandatory = $true)][ValidatePattern('^[a-fA-F0-9]{64}$')][string]$InstallerSHA256,
    [ValidateSet('qwen2.5-0.5b', 'qwen2.5-1.5b', 'qwen2.5-3b', 'gpt-oss-20b', 'gpt-oss-120b')][string]$Model = 'qwen2.5-1.5b',
    [ValidateRange(10, 3600)][int]$TurnTimeoutSeconds = 300,
    [ValidateRange(1, 65535)][int]$Port = 22,
    [string]$IdentityFile,
    [string]$ReportDirectory = (Join-Path (Get-Location) ('mini-fabrics-linux-' + [guid]::NewGuid().ToString('N')))
)

$ErrorActionPreference = 'Stop'
$Model = $Model.ToLowerInvariant()
. (Join-Path $PSScriptRoot 'Test-Common.ps1')
$artifact = Assert-FabricsArtifact -Path $Installer -ExpectedSHA256 $InstallerSHA256
$ssh = (Get-Command ssh -ErrorAction Stop).Source
$scp = (Get-Command scp -ErrorAction Stop).Source
$smoke = Join-Path (Split-Path -Parent $PSScriptRoot) 'smoke.py'
if (-not (Test-Path -LiteralPath $smoke -PathType Leaf)) { throw "Smoke runner not found at '$smoke'. Use this script from the source checkout." }
$report = New-FabricsEvidenceDirectory -Path $ReportDirectory
$destination = $UserName + '@' + $HostName
$remoteDirectory = '/tmp/mini-fabrics-test-' + [guid]::NewGuid().ToString('N')
$commonOptions = @('-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes', '-o', 'ConnectTimeout=15')
if ($IdentityFile) {
    $key = Get-Item -LiteralPath $IdentityFile -ErrorAction Stop
    if ($key.PSIsContainer) { throw 'IdentityFile must name an existing SSH private key.' }
    $commonOptions += @('-i', $key.FullName)
}
$sshOptions = $commonOptions + @('-p', "$Port")
$scpOptions = $commonOptions + @('-P', "$Port")
$status = 'failed'
$failure = $null
$remoteCreated = $false
try {
    $code = Invoke-FabricsLoggedCommand -Executable $ssh -Arguments ($sshOptions + @($destination, "umask 077 && mkdir $remoteDirectory")) -LogPath (Join-Path $report 'ssh-prepare.log')
    if ($code -ne 0) { throw "SSH setup failed (exit $code). Establish and verify the host key and key authentication before retrying." }
    $remoteCreated = $true
    $uploads = @(
        @{ Local = $artifact; Remote = 'installer' },
        @{ Local = $smoke; Remote = 'smoke.py' },
        @{ Local = (Join-Path $PSScriptRoot 'guest-test.sh'); Remote = 'guest-test.sh' }
    )
    foreach ($upload in $uploads) {
        $target = $destination + ':' + $remoteDirectory + '/' + $upload.Remote
        $code = Invoke-FabricsLoggedCommand -Executable $scp -Arguments ($scpOptions + @($upload.Local, $target)) -LogPath (Join-Path $report ('upload-' + $upload.Remote + '.log'))
        if ($code -ne 0) { throw "Transfer of $($upload.Remote) failed (exit $code)." }
    }
    # Every remote argument is a generated /tmp path, validated catalog token,
    # a validated hexadecimal digest, or a bounded integer. No caller-supplied
    # shell text is used.
    $command = "bash $remoteDirectory/guest-test.sh $remoteDirectory $($InstallerSHA256.ToLowerInvariant()) $Model $TurnTimeoutSeconds"
    $code = Invoke-FabricsLoggedCommand -Executable $ssh -Arguments ($sshOptions + @($destination, $command)) -LogPath (Join-Path $report 'guest-session.log')
    if ($code -ne 0) { throw "Guest install/smoke failed (exit $code); inspect the retained evidence." }
    $status = 'passed'
}
catch { $failure = $_.Exception.Message }
finally {
    if ($remoteCreated) {
        try {
            $source = $destination + ':' + $remoteDirectory + '/evidence'
            $copyCode = Invoke-FabricsLoggedCommand -Executable $scp -Arguments ($scpOptions + @('-r', $source, $report)) -LogPath (Join-Path $report 'download-evidence.log')
            if ($copyCode -ne 0) { throw "Evidence retrieval exited with $copyCode." }
        }
        catch {
            $status = 'failed'
            $failure = "$failure $($_.Exception.Message) Files remain in $remoteDirectory on the guest."
        }
    }
    [ordered]@{
        status = $status; host = $HostName; backend = 'cpu'; model = $Model
        turn_timeout_seconds = $TurnTimeoutSeconds
        installer_sha256 = $InstallerSHA256.ToLowerInvariant()
        guest_directory = $remoteDirectory; report_directory = $report; failure = $failure
        scope = 'Linux guest CPU installation, inference, isolated memory, persistence and shutdown; no GPU validation'
    } | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $report 'controller-report.json') -Encoding UTF8
}
if ($failure) { throw $failure }
Write-Host "Guest CPU verification passed. Evidence: $report"
