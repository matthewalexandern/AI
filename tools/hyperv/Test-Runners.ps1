#Requires -Version 5.1
# Controller protocol fixtures: no native installer, SSH, or GPU is executed.
[CmdletBinding()]
param()
$ErrorActionPreference = 'Stop'
$fixtureRoot = Join-Path ([IO.Path]::GetTempPath()) ('mini-fabrics-runners-' + [guid]::NewGuid().ToString('N'))
$fixtureScripts = Join-Path $fixtureRoot 'tools\hyperv'
New-Item -ItemType Directory -Path $fixtureScripts -Force | Out-Null
$previousOS = $env:OS
$previousTrace = $env:MINI_FABRICS_FIXTURE_TRACE
$fixtureExecutable = (Get-Process -Id $PID).Path
# Controllers only need a command's Source property; their actual invocation
# is recorded by the fixture helper below, never executed.
function Get-Command { param([string]$Name, [string]$ErrorAction) [pscustomobject]@{ Source = $fixtureExecutable } }
try {
    foreach ($name in @('Invoke-LinuxGuestTest.ps1', 'Invoke-WindowsHostTest.ps1', 'Test-Common.ps1', 'guest-test.sh')) {
        Copy-Item -LiteralPath (Join-Path $PSScriptRoot $name) -Destination $fixtureScripts
    }
    Set-Content -LiteralPath (Join-Path $fixtureRoot 'tools\smoke.py') -Value '# Fixture only; never executed.'
    Add-Content -LiteralPath (Join-Path $fixtureScripts 'Test-Common.ps1') -Value @'
function Invoke-FabricsLoggedCommand {
    param([string]$Executable, [string[]]$Arguments, [string]$LogPath)
    [ordered]@{ executable = $Executable; arguments = @($Arguments) } | ConvertTo-Json -Compress | Add-Content -LiteralPath $env:MINI_FABRICS_FIXTURE_TRACE -Encoding UTF8
    Set-Content -LiteralPath $LogPath -Value 'Fixture command only; no program was executed.'
    return 0
}
'@
    $artifact = Join-Path $fixtureRoot 'installer-fixture'
    Set-Content -LiteralPath $artifact -Value 'Non-executable installer fixture.'
    $checksum = (Get-FileHash -LiteralPath $artifact -Algorithm SHA256).Hash
    $env:OS = 'Windows_NT'
    $cases = @(
        @{ Kind = 'Windows'; ExpectedModel = 'qwen2.5-1.5b'; ExpectedTimeout = 300 },
        @{ Kind = 'Guest'; ExpectedModel = 'qwen2.5-1.5b'; ExpectedTimeout = 300 },
        @{ Kind = 'Windows'; Model = 'GPT-OSS-20B'; Timeout = 900; ExpectedModel = 'gpt-oss-20b'; ExpectedTimeout = 900 },
        @{ Kind = 'Guest'; Model = 'GPT-OSS-20B'; Timeout = 900; ExpectedModel = 'gpt-oss-20b'; ExpectedTimeout = 900 },
        @{ Kind = 'Windows'; Model = 'gpt-oss-120b'; Timeout = 3600; ExpectedModel = 'gpt-oss-120b'; ExpectedTimeout = 3600 },
        @{ Kind = 'Guest'; Model = 'gpt-oss-120b'; Timeout = 3600; ExpectedModel = 'gpt-oss-120b'; ExpectedTimeout = 3600 },
        @{ Kind = 'Windows'; Backend = 'cuda'; Timeout = 10; ExpectedModel = 'qwen2.5-1.5b'; ExpectedTimeout = 10 },
        @{ Kind = 'Guest'; Timeout = 10; ExpectedModel = 'qwen2.5-1.5b'; ExpectedTimeout = 10 }
    )
    $index = 0
    foreach ($case in $cases) {
        $index++
        $report = Join-Path $fixtureRoot "case-$index"
        $env:MINI_FABRICS_FIXTURE_TRACE = Join-Path $fixtureRoot "trace-$index.jsonl"
        $parameters = @{ Installer = $artifact; InstallerSHA256 = $checksum; ReportDirectory = $report }
        if ($case.ContainsKey('Model')) { $parameters.Model = $case.Model }
        if ($case.ContainsKey('Timeout')) { $parameters.TurnTimeoutSeconds = $case.Timeout }
        if ($case.Kind -eq 'Windows') {
            $parameters.Backend = $(if ($case.ContainsKey('Backend')) { $case.Backend } else { 'cpu' })
            & (Join-Path $fixtureScripts 'Invoke-WindowsHostTest.ps1') @parameters 6>$null | Out-Null
            $record = Get-Content -LiteralPath (Join-Path $report 'host-report.json') -Raw | ConvertFrom-Json
            $calls = @(Get-Content -LiteralPath $env:MINI_FABRICS_FIXTURE_TRACE | ForEach-Object { $_ | ConvertFrom-Json })
            $install = @($calls | Where-Object { $_.arguments -contains '--non-interactive' })[0].arguments
            if ($install[[array]::IndexOf($install, '--model') + 1] -ne $case.ExpectedModel) { throw 'Host model forwarding failed.' }
            $smoke = @($calls | Where-Object { $_.arguments -contains '--expect-backend' })[0].arguments
            if ($smoke[[array]::IndexOf($smoke, '--turn-timeout') + 1] -ne "$($case.ExpectedTimeout)") { throw 'Host timeout forwarding failed.' }
            if (($smoke -contains '--require-gpu') -ne ($parameters.Backend -ne 'cpu')) { throw 'Host GPU evidence gate changed.' }
        } else {
            $parameters.HostName = 'fixture.example'
            $parameters.UserName = 'tester'
            & (Join-Path $fixtureScripts 'Invoke-LinuxGuestTest.ps1') @parameters 6>$null | Out-Null
            $record = Get-Content -LiteralPath (Join-Path $report 'controller-report.json') -Raw | ConvertFrom-Json
            $calls = @(Get-Content -LiteralPath $env:MINI_FABRICS_FIXTURE_TRACE | ForEach-Object { $_ | ConvertFrom-Json })
            $commands = @($calls | ForEach-Object { $_.arguments } | Where-Object { $_ -like 'bash */guest-test.sh *' })
            if ($commands.Count -ne 1 -or -not $commands[0].EndsWith(" $($case.ExpectedModel) $($case.ExpectedTimeout)")) { throw 'Guest model/timeout SSH forwarding failed.' }
            foreach ($call in $calls) {
                if ($call.arguments -notcontains 'BatchMode=yes' -or $call.arguments -notcontains 'StrictHostKeyChecking=yes') { throw 'SSH trust controls changed.' }
            }
        }
        if ($record.status -ne 'passed' -or $record.model -ne $case.ExpectedModel -or $record.turn_timeout_seconds -ne $case.ExpectedTimeout) { throw 'Controller report lost model or timeout.' }
    }
    foreach ($name in @('Invoke-LinuxGuestTest.ps1', 'Invoke-WindowsHostTest.ps1')) {
        foreach ($invalid in @(9, 3601)) {
            $report = Join-Path $fixtureRoot ("invalid-$name-$invalid")
            $parameters = @{ Installer = 'must-not-be-read'; InstallerSHA256 = ('0' * 64); TurnTimeoutSeconds = $invalid; ReportDirectory = $report }
            if ($name -like '*Linux*') { $parameters.HostName = 'fixture.example'; $parameters.UserName = 'tester' } else { $parameters.Backend = 'cpu' }
            $rejected = $false
            try { & (Join-Path $fixtureScripts $name) @parameters | Out-Null }
            catch { $rejected = $_.FullyQualifiedErrorId -like 'ParameterArgumentValidationError*' }
            if (-not $rejected -or (Test-Path -LiteralPath $report)) { throw 'Invalid turn timeout was not rejected before side effects.' }
        }
    }
    Write-Host 'Eight controller forwarding/report fixtures and four timeout rejection cases passed; no native Windows/SSH/GPU execution.'
}
finally {
    $env:OS = $previousOS
    $env:MINI_FABRICS_FIXTURE_TRACE = $previousTrace
    Remove-Item -LiteralPath $fixtureRoot -Recurse -Force
}
