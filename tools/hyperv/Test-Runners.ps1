#Requires -Version 5.1
# Controller protocol fixtures: no native installer, SSH, or GPU is executed.
[CmdletBinding()]
param()
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'Test-Common.ps1')
$fixtureRoot = Join-Path ([IO.Path]::GetTempPath()) ('mini-fabrics-runners-' + [guid]::NewGuid().ToString('N'))
$fixtureScripts = Join-Path $fixtureRoot 'tools\hyperv'
New-Item -ItemType Directory -Path $fixtureScripts -Force | Out-Null
$previousOS = $env:OS
$previousTrace = $env:MINI_FABRICS_FIXTURE_TRACE
$previousGuestID = $env:MINI_FABRICS_FIXTURE_GUEST_ID
$previousGuestArchitecture = $env:MINI_FABRICS_FIXTURE_GUEST_ARCH
$previousGuestAddress = $env:MINI_FABRICS_FIXTURE_GUEST_ADDRESS
$fixtureExecutable = (Get-Process -Id $PID).Path
# Controllers only need a command's Source property; their actual invocation
# is recorded by the fixture helper below, never executed.
function Get-Command { param([string]$Name, [string]$ErrorAction) [pscustomobject]@{ Source = $fixtureExecutable } }
function Get-VM { param([string]$Name, [string]$ErrorAction) [pscustomobject]@{ Name = $Name; Id = '11111111-2222-3333-4444-555555555555'; State = 'Running'; Generation = 2; MemoryAssigned = 6GB; ProcessorCount = 2 } }
function Get-VMNetworkAdapter { param($VM, [string]$ErrorAction) [pscustomobject]@{ IPAddresses = @($env:MINI_FABRICS_FIXTURE_GUEST_ADDRESS) } }
try {
    $catalogPath = Join-Path (Split-Path -Parent (Split-Path -Parent $PSScriptRoot)) 'installer/models.json'
    $catalog = Get-Content -LiteralPath $catalogPath -Raw | ConvertFrom-Json
    foreach ($entry in $catalog) {
        if ((Get-FabricsExpectedModelFilename $entry.name) -cne ($entry.name + '-' + $entry.sha256 + '.gguf')) { throw 'Guest evidence model pins differ from the installer catalog.' }
    }
    foreach ($name in @('Invoke-LinuxGuestTest.ps1', 'Invoke-WindowsHostTest.ps1', 'Test-Common.ps1', 'guest-test.sh')) {
        Copy-Item -LiteralPath (Join-Path $PSScriptRoot $name) -Destination $fixtureScripts
    }
    Set-Content -LiteralPath (Join-Path $fixtureRoot 'tools\smoke.py') -Value '# Fixture only; never executed.'
    Add-Content -LiteralPath (Join-Path $fixtureScripts 'Test-Common.ps1') -Value @'
function Invoke-FabricsLoggedCommand {
    param([string]$Executable, [string[]]$Arguments, [string]$LogPath)
    [ordered]@{ executable = $Executable; arguments = @($Arguments) } | ConvertTo-Json -Compress | Add-Content -LiteralPath $env:MINI_FABRICS_FIXTURE_TRACE -Encoding UTF8
    Set-Content -LiteralPath $LogPath -Value 'Fixture command only; no program was executed.'
    if ($Arguments -contains '-r') {
        $evidence = Join-Path $ReportDirectory 'evidence'
        New-Item -ItemType Directory -Path $evidence | Out-Null
        [ordered]@{
            status = 'passed'; exit_code = 0; last_stage = 'complete'; backend = 'cpu'; gpu_validated = $false
            model = $Model; turn_timeout_seconds = $TurnTimeoutSeconds; installer_sha256 = $InstallerSHA256.ToLowerInvariant()
            architecture = $env:MINI_FABRICS_FIXTURE_GUEST_ARCH; os_release = @{ ID = $env:MINI_FABRICS_FIXTURE_GUEST_ID; VERSION_ID = 'fixture' }
        } | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $evidence 'guest-report.json') -Encoding UTF8
        [ordered]@{
            status = 'passed'; runtime_checks = 'passed'; mode = 'balanced'; backend = 'cpu'
            model = (Get-FabricsExpectedModelFilename $Model); arithmetic = @{ answer = '4' }
            backend_evidence = @{ host_os = 'Linux'; host_arch = $env:MINI_FABRICS_FIXTURE_GUEST_ARCH; configured_backend = 'cpu'; expected_backend = 'cpu'; offload_confirmed = $false; offloaded_layers = 0 }
            recall = @{ answer = 'Neptune'; memories = @(@{ id = 2; content = 'Neptune' }) }; persisted_turn = @{ answer = 'Neptune'; recall_ids = @(2) }
            reopen = 'persisted turn unchanged'; shutdown = 'parent exited, inference listeners closed, POSIX process group empty'
        } | ConvertTo-Json -Depth 6 | Set-Content -LiteralPath (Join-Path $evidence 'smoke.json') -Encoding UTF8
    }
    return 0
}
'@
    $artifact = Join-Path $fixtureRoot 'installer-fixture'
    Set-Content -LiteralPath $artifact -Value 'Non-executable installer fixture.'
    $checksum = (Get-FileHash -LiteralPath $artifact -Algorithm SHA256).Hash
    $knownHosts = Join-Path $fixtureRoot 'verified known_hosts'
    Set-Content -LiteralPath $knownHosts -Value '# Fixture host-key file only.'
    $env:OS = 'Windows_NT'
    $env:MINI_FABRICS_FIXTURE_GUEST_ID = 'ubuntu'
    $env:MINI_FABRICS_FIXTURE_GUEST_ARCH = 'x86_64'
    $env:MINI_FABRICS_FIXTURE_GUEST_ADDRESS = '192.0.2.10'
    $cases = @(
        @{ Kind = 'Windows'; ExpectedModel = 'qwen2.5-1.5b'; ExpectedTimeout = 300 },
        @{ Kind = 'Guest'; ExpectedModel = 'qwen2.5-1.5b'; ExpectedTimeout = 300 },
        @{ Kind = 'Windows'; Model = 'GPT-OSS-20B'; Timeout = 900; ExpectedModel = 'gpt-oss-20b'; ExpectedTimeout = 900 },
        @{ Kind = 'Guest'; Model = 'GPT-OSS-20B'; Timeout = 900; ExpectedModel = 'gpt-oss-20b'; ExpectedTimeout = 900 },
        @{ Kind = 'Windows'; Model = 'gpt-oss-120b'; Timeout = 3600; ExpectedModel = 'gpt-oss-120b'; ExpectedTimeout = 3600 },
        @{ Kind = 'Guest'; Model = 'gpt-oss-120b'; Timeout = 3600; ExpectedModel = 'gpt-oss-120b'; ExpectedTimeout = 3600 },
        @{ Kind = 'Windows'; Backend = 'cuda'; Timeout = 10; ExpectedModel = 'qwen2.5-1.5b'; ExpectedTimeout = 10 },
        @{ Kind = 'Guest'; Timeout = 10; ExpectedModel = 'qwen2.5-1.5b'; ExpectedTimeout = 10 },
        @{ Kind = 'Guest'; Model = 'qwen2.5-0.5b'; ExpectedModel = 'qwen2.5-0.5b'; ExpectedTimeout = 300 },
        @{ Kind = 'Guest'; Model = 'qwen2.5-3b'; ExpectedModel = 'qwen2.5-3b'; ExpectedTimeout = 300 }
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
            $parameters.KnownHostsFile = $knownHosts
            & (Join-Path $fixtureScripts 'Invoke-LinuxGuestTest.ps1') @parameters 6>$null | Out-Null
            $record = Get-Content -LiteralPath (Join-Path $report 'controller-report.json') -Raw | ConvertFrom-Json
            $calls = @(Get-Content -LiteralPath $env:MINI_FABRICS_FIXTURE_TRACE | ForEach-Object { $_ | ConvertFrom-Json })
            $commands = @($calls | ForEach-Object { $_.arguments } | Where-Object { $_ -like 'bash */guest-test.sh *' })
            if ($commands.Count -ne 1 -or -not $commands[0].EndsWith(" $($case.ExpectedModel) $($case.ExpectedTimeout)")) { throw 'Guest model/timeout SSH forwarding failed.' }
            foreach ($call in $calls) {
                if ($call.arguments -notcontains 'BatchMode=yes' -or $call.arguments -notcontains 'StrictHostKeyChecking=yes') { throw 'SSH trust controls changed.' }
                $configIndex = [array]::IndexOf($call.arguments, '-F')
                if ($configIndex -lt 0 -or $call.arguments[$configIndex + 1] -ne (Join-Path $report 'ssh-known-hosts.config')) { throw 'SSH/SCP did not use the fixed verified host-key config.' }
            }
        }
        if ($record.status -ne 'passed' -or $record.model -ne $case.ExpectedModel -or $record.turn_timeout_seconds -ne $case.ExpectedTimeout) { throw 'Controller report lost model or timeout.' }
    }
    # Fresh catalog installs use exact name-SHA256 filenames. Wildcards or
    # publisher/local aliases must not identify a different model as this run.
    $modelReport = Join-Path $fixtureRoot 'case-2'
    $smokePath = Join-Path $modelReport 'evidence/smoke.json'
    $originalSmoke = Get-Content -LiteralPath $smokePath -Raw
    foreach ($invalidModel in @('qwen2.5-1.5b.gguf', 'qwen2.5-1.5b-instruct-q4_k_m.gguf', ('qwen2.5-1.5b-' + ('0' * 64) + '.gguf'), (Get-FabricsExpectedModelFilename 'gpt-oss-20b'))) {
        $smoke = $originalSmoke | ConvertFrom-Json
        $smoke.model = $invalidModel
        $smoke | ConvertTo-Json -Depth 6 | Set-Content -LiteralPath $smokePath -Encoding UTF8
        $rejected = $false
        try { $null = Assert-FabricsGuestEvidence -ReportDirectory $modelReport -InstallerSHA256 $checksum -Model 'qwen2.5-1.5b' -TurnTimeoutSeconds 300 }
        catch { $rejected = $true }
        if (-not $rejected) { throw 'A wrong model filename/hash was accepted as fresh catalog evidence.' }
    }
    Set-Content -LiteralPath $smokePath -Value $originalSmoke -Encoding UTF8
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
    # A named Hyper-V claim requires both endpoint identity and retrieved OS/CPU
    # evidence. These mocks never contact SSH or invoke a Hyper-V cmdlet.
    $hypervCases = @(
        @{ Name = 'matched'; Host = '192.0.2.10'; ID = 'ubuntu'; Architecture = 'x86_64'; Passed = $true },
        @{ Name = 'wrong-ip'; Host = '192.0.2.11'; ID = 'ubuntu'; Architecture = 'x86_64'; Passed = $false },
        @{ Name = 'compatible-not-rhel'; Host = '192.0.2.10'; ID = 'almalinux'; Architecture = 'x86_64'; Distribution = 'RHEL'; Passed = $false },
        @{ Name = 'wrong-architecture'; Host = '192.0.2.10'; ID = 'ubuntu'; Architecture = 'aarch64'; Passed = $false }
    )
    foreach ($case in $hypervCases) {
        $report = Join-Path $fixtureRoot ('hyperv-' + $case.Name)
        $env:MINI_FABRICS_FIXTURE_TRACE = Join-Path $fixtureRoot ('trace-hyperv-' + $case.Name + '.jsonl')
        $env:MINI_FABRICS_FIXTURE_GUEST_ID = $case.ID
        $env:MINI_FABRICS_FIXTURE_GUEST_ARCH = $case.Architecture
        $parameters = @{
            HostName = $case.Host; UserName = 'tester'; Installer = $artifact; InstallerSHA256 = $checksum
            ReportDirectory = $report; VMName = 'MiniFabrics-Ubuntu'; ExpectedArchitecture = 'x86_64'
            KnownHostsFile = $knownHosts
            ExpectedDistribution = $(if ($case.ContainsKey('Distribution')) { $case.Distribution } else { 'Ubuntu' })
        }
        $succeeded = $true
        try { & (Join-Path $fixtureScripts 'Invoke-LinuxGuestTest.ps1') @parameters 6>$null | Out-Null }
        catch { $succeeded = $false }
        $record = Get-Content -LiteralPath (Join-Path $report 'controller-report.json') -Raw | ConvertFrom-Json
        if ($succeeded -ne $case.Passed -or (($record.status -eq 'passed') -ne $case.Passed)) { throw "Hyper-V evidence fixture failed: $($case.Name)" }
        if ($case.Passed -and ($record.hyperv_vm.ssh_ipv4 -ne $case.Host -or $record.verified_guest.os_release.ID -ne $case.ID -or $record.evidence_kind -ne 'named Hyper-V guest CPU test')) { throw 'Named Hyper-V result lost VM/OS identity.' }
        if ($case.Name -eq 'wrong-ip' -and (Test-Path -LiteralPath $env:MINI_FABRICS_FIXTURE_TRACE)) { throw 'A mismatched VM address reached SSH.' }
    }
    Write-Host 'Ten controller forwarding/report fixtures, five catalog filename pins, four wrong-model rejections, and four timeout rejection cases passed; no native Windows/SSH/GPU execution.'
    Write-Host 'Four named-VM IP/distribution/architecture evidence fixtures passed; no Hyper-V execution.'
}
finally {
    $env:OS = $previousOS
    $env:MINI_FABRICS_FIXTURE_TRACE = $previousTrace
    $env:MINI_FABRICS_FIXTURE_GUEST_ID = $previousGuestID
    $env:MINI_FABRICS_FIXTURE_GUEST_ARCH = $previousGuestArchitecture
    $env:MINI_FABRICS_FIXTURE_GUEST_ADDRESS = $previousGuestAddress
    Remove-Item -LiteralPath $fixtureRoot -Recurse -Force
}
