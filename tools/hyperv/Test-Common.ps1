# Helpers shared by the opt-in native test runners. No side effects on import.
function Invoke-FabricsLoggedCommand {
    param(
        [Parameter(Mandatory = $true)][string]$Executable,
        [Parameter(Mandatory = $true)][string[]]$Arguments,
        [Parameter(Mandatory = $true)][string]$LogPath
    )
    $previousPreference = $ErrorActionPreference
    $PSNativeCommandUseErrorActionPreference = $false
    # Native commands update the global automatic variable. A local assignment
    # would shadow that update and hide the real process result inside a function.
    $global:LASTEXITCODE = $null
    try {
        # Windows PowerShell 5.1 wraps native stderr in ErrorRecord objects.
        # Preserve it as log text and use the process exit code as the result.
        $ErrorActionPreference = 'Continue'
        & $Executable @Arguments 2>&1 | ForEach-Object { "$_" } | Tee-Object -FilePath $LogPath -ErrorAction Stop | Out-Host
        $code = $global:LASTEXITCODE
    }
    finally { $ErrorActionPreference = $previousPreference }
    if ($null -eq $code) { throw "Could not launch '$Executable'; no process exit code was returned." }
    return [int]$code
}

function Assert-FabricsArtifact {
    param([string]$Path, [string]$ExpectedSHA256)
    $artifact = Get-Item -LiteralPath $Path -ErrorAction Stop
    if ($artifact.PSIsContainer) { throw 'The installer must be an existing regular file.' }
    if ((Get-FileHash -LiteralPath $artifact.FullName -Algorithm SHA256).Hash -ne $ExpectedSHA256) {
        throw 'Installer SHA256 does not match the supplied trusted release checksum.'
    }
    return $artifact.FullName
}

function New-FabricsEvidenceDirectory {
    param([string]$Path)
    $fullPath = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($Path)
    if (Test-Path -LiteralPath $fullPath) { throw "Preserving existing evidence directory '$fullPath'; choose a new -ReportDirectory." }
    New-Item -ItemType Directory -Path $fullPath -ErrorAction Stop | Out-Null
    return $fullPath
}
