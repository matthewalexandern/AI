#Requires -Version 5.1
# Executes only a fixed Python argv recorder; no SSH, UAC, or VM operations.
[CmdletBinding()]
param()
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'Test-Common.ps1')
$fixtureRoot = Join-Path ([IO.Path]::GetTempPath()) ('mini-fabrics-native-args-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $fixtureRoot | Out-Null
try {
    $python = (Get-Command python -ErrorAction Stop).Source
    $recorder = Join-Path $fixtureRoot 'record_argv.py'
    @'
import json
from pathlib import Path
import sys
Path(sys.argv[1]).write_text(json.dumps(sys.argv[2:]), encoding="utf-8")
'@ | Set-Content -LiteralPath $recorder -Encoding UTF8
    $directory = Join-Path $fixtureRoot "known hosts' space"
    New-Item -ItemType Directory -Path $directory | Out-Null
    $knownHosts = Join-Path $directory 'known_hosts'
    Set-Content -LiteralPath $knownHosts -Value '# Fixture only; no SSH keys or connections.' -Encoding UTF8
    $report = Join-Path $directory 'report evidence'
    New-Item -ItemType Directory -Path $report | Out-Null
    $config = New-FabricsKnownHostsConfig -ReportDirectory $report -KnownHostsPath $knownHosts
    $expectedConfig = "Host *`n    UserKnownHostsFile `"$($knownHosts.Replace('\', '/'))`"`n"
    $bytes = [IO.File]::ReadAllBytes($config)
    if (($bytes.Length -ge 3 -and $bytes[0] -eq 239 -and $bytes[1] -eq 187 -and $bytes[2] -eq 191) -or
        [Text.Encoding]::UTF8.GetString($bytes) -cne $expectedConfig) { throw 'Fixed SSH config must contain exactly the quoted host-key file in UTF-8 without a BOM.' }
    $preserved = $false
    try { $null = New-FabricsKnownHostsConfig -ReportDirectory $report -KnownHostsPath $knownHosts }
    catch { $preserved = $true }
    if (-not $preserved -or [Text.Encoding]::UTF8.GetString([IO.File]::ReadAllBytes($config)) -cne $expectedConfig) { throw 'Existing SSH config was overwritten.' }
    $modes = $(if ($PSVersionTable.PSVersion -ge [version]'7.3') { @('Standard','Legacy') } else { @('Legacy') })
    foreach ($mode in $modes) {
        if ($PSVersionTable.PSVersion -ge [version]'7.3') { $PSNativeCommandArgumentPassing = $mode }
        $output = Join-Path $fixtureRoot ($mode + '.json')
        $code = Invoke-FabricsLoggedCommand -Executable $python -Arguments @($recorder, $output, '-F', $config) -LogPath (Join-Path $fixtureRoot ($mode + '.log'))
        if ($code -ne 0) { throw "Fixed native argv recorder failed for $mode." }
        $received = Get-Content -LiteralPath $output -Raw -Encoding UTF8 | ConvertFrom-Json
        if ($received.Count -ne 2 -or $received[0] -cne '-F' -or $received[1] -cne $config) { throw "SSH config path changed in $mode native argument handling." }
    }
    Write-Host "Fixed SSH config and native -F path round-trip passed for $($modes -join ', ') with spaces and an apostrophe. No SSH or host changes."
}
finally { Remove-Item -LiteralPath $fixtureRoot -Recurse -Force }
