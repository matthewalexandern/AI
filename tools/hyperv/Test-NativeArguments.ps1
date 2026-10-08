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
    $expected = 'UserKnownHostsFile="' + $knownHosts.Replace('\', '/') + '"'
    $modes = $(if ($PSVersionTable.PSVersion -ge [version]'7.3') { @('Standard','Legacy') } else { @('Legacy') })
    foreach ($mode in $modes) {
        if ($PSVersionTable.PSVersion -ge [version]'7.3') { $PSNativeCommandArgumentPassing = $mode }
        $option = Get-FabricsKnownHostsOption -Path $knownHosts
        $output = Join-Path $fixtureRoot ($mode + '.json')
        $code = Invoke-FabricsLoggedCommand -Executable $python -Arguments @($recorder, $output, '-o', $option) -LogPath (Join-Path $fixtureRoot ($mode + '.log'))
        if ($code -ne 0) { throw "Fixed native argv recorder failed for $mode." }
        $received = Get-Content -LiteralPath $output -Raw -Encoding UTF8 | ConvertFrom-Json
        if ($received.Count -ne 2 -or $received[0] -cne '-o' -or $received[1] -cne $expected) {
            throw "KnownHostsFile path quotes changed in $mode native argument handling."
        }
    }
    Write-Host "KnownHostsFile native argv round-trip passed for $($modes -join ', ') with a path containing spaces and an apostrophe. No SSH or host changes."
}
finally { Remove-Item -LiteralPath $fixtureRoot -Recurse -Force }
