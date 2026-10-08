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

function Get-FabricsKnownHostsOption {
    param([Parameter(Mandatory = $true)][string]$Path)
    if ([string]::IsNullOrEmpty($Path) -or $Path -match '["\x00-\x1f]') { throw 'Known-host paths cannot contain quotes or control characters.' }
    $option = 'UserKnownHostsFile="' + $Path.Replace('\', '/') + '"'
    # PowerShell 5.1 removes embedded native-argument quotes. Preserve the
    # quotes required by OpenSSH's list-of-files parser when using that mode.
    $argumentMode = Get-Variable -Name PSNativeCommandArgumentPassing -ValueOnly -ErrorAction SilentlyContinue
    if ($PSVersionTable.PSVersion -lt [version]'7.3' -or $argumentMode -eq 'Legacy') { $option = $option.Replace('"', '\"') }
    return $option
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

function Get-FabricsHyperVBinding {
    param([string]$Name, [string]$HostName)
    if ($env:OS -ne 'Windows_NT') { throw 'Hyper-V guest identity requires the actual Windows host.' }
    $vm = Get-VM -Name $Name -ErrorAction Stop
    if ($vm.Name -ne $Name -or "$($vm.State)" -ne 'Running' -or $vm.Generation -ne 2) {
        throw 'The named Hyper-V guest must already be a running Generation 2 VM. Existing VM power states are preserved.'
    }
    $reported = @(Get-VMNetworkAdapter -VM $vm -ErrorAction Stop | ForEach-Object { $_.IPAddresses } | Where-Object {
        $address = $null
        [Net.IPAddress]::TryParse("$_", [ref]$address) -and $address.AddressFamily -eq [Net.Sockets.AddressFamily]::InterNetwork
    })
    if ($reported.Count -eq 0) { throw 'Hyper-V reports no guest IPv4 address; VM identity cannot be established. Check integration services and guest networking.' }
    $literal = $null
    if ([Net.IPAddress]::TryParse($HostName, [ref]$literal)) { $resolved = @($literal.ToString()) }
    else { $resolved = @([Net.Dns]::GetHostAddresses($HostName) | ForEach-Object { $_.ToString() }) }
    $matched = @($resolved | Where-Object { $reported -contains $_ })
    # Pass the verified literal to SSH so a second DNS lookup cannot change it.
    if ($matched.Count -ne 1) { throw 'The SSH destination must resolve to exactly one IPv4 address reported for the named Hyper-V VM.' }
    return [pscustomobject][ordered]@{
        name = $vm.Name; id = "$($vm.Id)"; generation = $vm.Generation
        state = "$($vm.State)"; memory_assigned = $vm.MemoryAssigned
        processor_count = $vm.ProcessorCount; reported_ipv4 = @($reported)
        ssh_ipv4 = $matched[0]; binding = 'Hyper-V reported address and strict SSH host-key verification'
    }
}

function Get-FabricsExpectedModelFilename {
    param([string]$Model)
    # downloadModel writes name-SHA256.gguf, rather than the publisher basename.
    # Test-Runners verifies these pins against the installer's trusted catalog.
    $hashes = @{
        'gpt-oss-20b' = '27cd6c432c7672cb812a92f611cf3ba7bbc35928262bb1e1253ff4ee6ae35901'
        'gpt-oss-120b' = '582bd40f6886200101f4c4ed9f25f3fe80cc14c86e9e2b37746cd8904a0c622d'
        'qwen2.5-0.5b' = '74a4da8c9fdbcd15bd1f6d01d621410d31c6fc00986f5eb687824e7b93d7a9db'
        'qwen2.5-1.5b' = '6a1a2eb6d15622bf3c96857206351ba97e1af16c30d7a74ee38970e434e9407e'
        'qwen2.5-3b' = '626b4a6678b86442240e33df819e00132d3ba7dddfe1cdc4fbb18e0a9615c62d'
    }
    if (-not $hashes.ContainsKey($Model)) { throw 'Unknown pinned catalog model in guest evidence.' }
    return ($Model + '-' + $hashes[$Model] + '.gguf')
}

function Assert-FabricsGuestEvidence {
    param(
        [string]$ReportDirectory, [string]$ExpectedDistribution, [string]$ExpectedArchitecture,
        [string]$InstallerSHA256, [string]$Model, [int]$TurnTimeoutSeconds
    )
    $evidence = Join-Path $ReportDirectory 'evidence'
    foreach ($name in @('guest-report.json', 'smoke.json')) {
        $file = Get-Item -LiteralPath (Join-Path $evidence $name) -ErrorAction Stop
        if ($file.PSIsContainer -or $file.Length -gt 16MB) { throw 'Guest evidence must be bounded JSON files.' }
    }
    $guest = Get-Content -LiteralPath (Join-Path $evidence 'guest-report.json') -Raw -Encoding UTF8 | ConvertFrom-Json
    $smoke = Get-Content -LiteralPath (Join-Path $evidence 'smoke.json') -Raw -Encoding UTF8 | ConvertFrom-Json
    if ($guest.status -ne 'passed' -or $guest.exit_code -ne 0 -or $guest.last_stage -ne 'complete' -or
        $guest.backend -ne 'cpu' -or $guest.gpu_validated -ne $false -or
        $guest.installer_sha256 -ne $InstallerSHA256.ToLowerInvariant() -or
        $guest.model -ne $Model -or $guest.turn_timeout_seconds -ne $TurnTimeoutSeconds) {
        throw 'Retrieved guest report does not prove completion of this installer/model/deadline test.'
    }
    if ($ExpectedDistribution) {
        $expectedID = @{ Ubuntu = 'ubuntu'; RHEL = 'rhel' }[$ExpectedDistribution]
        if ($guest.os_release.ID -ne $expectedID) { throw "Guest distribution differs from expected $ExpectedDistribution; a compatible distribution is not actual RHEL evidence." }
    }
    if ($ExpectedArchitecture -and $guest.architecture -ne $ExpectedArchitecture) { throw 'Guest architecture differs from the requested native target.' }
    if ($smoke.runtime_checks -ne 'passed' -or $smoke.status -notin @('passed', 'passed_with_model_warnings') -or
        $smoke.mode -ne 'balanced' -or $smoke.backend -ne 'cpu' -or
        $smoke.model -cne (Get-FabricsExpectedModelFilename $Model) -or $smoke.arithmetic.answer -notmatch '\b4\b' -or
        $smoke.backend_evidence.host_os -ne 'Linux' -or $smoke.backend_evidence.host_arch -ne $guest.architecture -or
        $smoke.backend_evidence.expected_backend -ne 'cpu' -or $smoke.backend_evidence.configured_backend -ne 'cpu' -or
        $smoke.backend_evidence.offload_confirmed -ne $false -or $smoke.backend_evidence.offloaded_layers -ne 0 -or
        $smoke.reopen -ne 'persisted turn unchanged' -or $smoke.shutdown -notmatch 'parent exited.*listeners closed' -or
        $smoke.persisted_turn.answer -ne $smoke.recall.answer -or $smoke.recall.answer -notmatch '(?i)neptune' -or
        @($smoke.persisted_turn.recall_ids).Count -eq 0) {
        throw 'Retrieved CPU smoke evidence is incomplete or inconsistent with the guest report.'
    }
    $memoryIDs = @($smoke.recall.memories | Where-Object { $_.content -match '(?i)neptune' } | ForEach-Object { $_.id })
    if (@($smoke.persisted_turn.recall_ids | Where-Object { $memoryIDs -contains $_ }).Count -eq 0) { throw 'Retrieved smoke evidence does not retain provenance for the recalled Neptune preference.' }
    return [pscustomobject][ordered]@{
        os_release = $guest.os_release; architecture = $guest.architecture
        guest_report_sha256 = (Get-FileHash -LiteralPath (Join-Path $evidence 'guest-report.json') -Algorithm SHA256).Hash.ToLowerInvariant()
        smoke_sha256 = (Get-FileHash -LiteralPath (Join-Path $evidence 'smoke.json') -Algorithm SHA256).Hash.ToLowerInvariant()
    }
}
