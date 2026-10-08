#Requires -Version 5.1
<#
.SYNOPSIS
Run a fixed native-test plan with one UAC elevation for this process session.
.DESCRIPTION
Validates trusted installers, ISOs, paths, and the closed JSON plan before UAC.
Does not register a runner, change execution policy, restart, or delete VMs.
A required host restart ends this session; later work needs a new launch.
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$PlanPath,
    [switch]$ValidateOnly,
    [switch]$Elevated,
    [ValidatePattern('^[a-fA-F0-9]{64}$')][string]$ExpectedPlanSHA256,
    [ValidatePattern('^[a-fA-F0-9]{64}$')][string]$ExpectedScriptsSHA256
)
$ErrorActionPreference = 'Stop'
$matrixScriptRoot = $PSScriptRoot

function Assert-FabricsMatrixKeys {
    param($Value, [string[]]$Allowed, [string[]]$Required, [string]$Context)
    if ($null -eq $Value -or $Value -isnot [pscustomobject]) { throw "$Context must be a JSON object." }
    foreach ($key in $Value.PSObject.Properties.Name) {
        if ($Allowed -cnotcontains $key) { throw "Unknown $Context field '$key'; executable commands are not accepted." }
    }
    foreach ($key in $Required) {
        if ($Value.PSObject.Properties.Name -cnotcontains $key) { throw "Missing $Context field '$key'." }
    }
}
function Assert-FabricsMatrixText {
    param($Value, [string]$Pattern, [string]$Context)
    if ($Value -isnot [string] -or $Value.Length -gt 4096 -or $Value -notmatch $Pattern) { throw "Invalid $Context." }
}
function Assert-FabricsMatrixInteger {
    param($Value, [long]$Minimum, [long]$Maximum, [string]$Context)
    if ($Value -isnot [int] -and $Value -isnot [long] -and $Value -isnot [double] -and $Value -isnot [decimal]) { throw "$Context must be an integer." }
    if ($Value -lt $Minimum -or $Value -gt $Maximum -or [math]::Floor($Value) -ne $Value) { throw "Invalid $Context bounds." }
}
function Resolve-FabricsMatrixPath {
    param($Value, [string]$Base, [switch]$ExistingFile)
    Assert-FabricsMatrixText $Value '^[^\x00-\x1f]+$' 'local path'
    $candidate = $(if ([IO.Path]::IsPathRooted($Value)) { $Value } else { Join-Path $Base $Value })
    $provider = $null; $drive = $null
    $resolved = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($candidate, [ref]$provider, [ref]$drive)
    if ($provider.Name -ne 'FileSystem') { throw 'Plan paths must use the filesystem provider.' }
    if ($ExistingFile) {
        $item = Get-Item -LiteralPath $resolved -ErrorAction Stop
        if ($item.PSIsContainer) { throw "Expected a regular file: $resolved" }
        $resolved = $item.FullName
    }
    return $resolved
}
function Assert-FabricsMatrixFileHash {
    param([string]$Path, $Digest)
    Assert-FabricsMatrixText $Digest '^[a-fA-F0-9]{64}$' 'trusted SHA256'
    if ((Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash -ne $Digest) { throw "SHA256 mismatch; preserving all resources: $Path" }
}
function Set-FabricsMatrixTestDefaults {
    param($Test)
    if ($Test.PSObject.Properties.Name -notcontains 'model') { $Test | Add-Member NoteProperty model 'gpt-oss-20b' }
    if ($Test.PSObject.Properties.Name -notcontains 'turn_timeout_seconds') { $Test | Add-Member NoteProperty turn_timeout_seconds 1800 }
    Assert-FabricsMatrixText $Test.model '^(qwen2\.5-(0\.5b|1\.5b|3b)|gpt-oss-(20b|120b))$' 'catalog model'
    Assert-FabricsMatrixInteger $Test.turn_timeout_seconds 10 3600 'turn timeout'
}
function Read-FabricsMatrixPlan {
    param([string]$Path)
    $file = Get-Item -LiteralPath $Path -ErrorAction Stop
    if ($file.PSIsContainer -or $file.Length -gt 65536) { throw 'Plan must be a JSON file of at most 64 KiB.' }
    $plan = Get-Content -LiteralPath $file.FullName -Raw -Encoding UTF8 | ConvertFrom-Json -ErrorAction Stop
    Assert-FabricsMatrixKeys $plan @('schema_version','enable_hyperv','report_root','windows','provision','guests') @('schema_version','enable_hyperv','report_root') 'plan'
    Assert-FabricsMatrixInteger $plan.schema_version 1 1 'schema version'
    if ($plan.enable_hyperv -isnot [bool]) { throw 'enable_hyperv must be a JSON boolean.' }
    $base = $file.DirectoryName
    $plan.report_root = Resolve-FabricsMatrixPath $plan.report_root $base
    if (Test-Path -LiteralPath $plan.report_root) { throw "Preserving existing evidence path '$($plan.report_root)'; choose a new report_root." }
    foreach ($list in @('provision','guests')) {
        if ($plan.PSObject.Properties.Name -notcontains $list) { $plan | Add-Member NoteProperty $list @() }
        if ($plan.$list -isnot [array] -or $plan.$list.Count -gt 2) { throw "$list must be a JSON array containing at most two entries." }
    }
    if ($null -ne $plan.windows) {
        Assert-FabricsMatrixKeys $plan.windows @('installer','installer_sha256','model','turn_timeout_seconds') @('installer','installer_sha256') 'windows'
        $plan.windows.installer = Resolve-FabricsMatrixPath $plan.windows.installer $base -ExistingFile
        Assert-FabricsMatrixFileHash $plan.windows.installer $plan.windows.installer_sha256
        Set-FabricsMatrixTestDefaults $plan.windows
    }
    $names = @()
    foreach ($vm in $plan.provision) {
        Assert-FabricsMatrixKeys $vm @('distribution','iso_path','iso_sha256','name','vm_root','switch_name','memory_bytes','processors','disk_bytes','start') @('distribution','iso_path','iso_sha256','name','vm_root','switch_name','memory_bytes','processors','disk_bytes','start') 'provision'
        Assert-FabricsMatrixText $vm.distribution '^(Ubuntu|RHEL)$' 'distribution'
        Assert-FabricsMatrixText $vm.name '^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$' 'VM name'
        if ($names -contains $vm.name) { throw 'Duplicate provisioning VM name.' }; $names += $vm.name
        Assert-FabricsMatrixText $vm.switch_name '^[^\x00-\x1f]{1,128}$' 'virtual switch'
        if ($vm.start -isnot [bool]) { throw 'start must be a JSON boolean.' }
        Assert-FabricsMatrixInteger $vm.memory_bytes 2GB 128GB 'VM memory'
        Assert-FabricsMatrixInteger $vm.processors 1 32 'VM processor count'
        Assert-FabricsMatrixInteger $vm.disk_bytes 32GB 512GB 'VM disk capacity'
        $vm.iso_path = Resolve-FabricsMatrixPath $vm.iso_path $base -ExistingFile
        if ([IO.Path]::GetExtension($vm.iso_path) -ne '.iso') { throw 'Provisioning media must be a local .iso file.' }
        Assert-FabricsMatrixFileHash $vm.iso_path $vm.iso_sha256
        $vm.vm_root = Resolve-FabricsMatrixPath $vm.vm_root $base
    }
    $names = @()
    foreach ($guest in $plan.guests) {
        Assert-FabricsMatrixKeys $guest @('vm_name','distribution','architecture','host_name','user_name','port','identity_file','known_hosts_file','installer','installer_sha256','model','turn_timeout_seconds') @('vm_name','distribution','architecture','host_name','user_name','known_hosts_file','installer','installer_sha256') 'guest'
        Assert-FabricsMatrixText $guest.vm_name '^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$' 'VM name'
        if ($names -contains $guest.vm_name) { throw 'Duplicate test VM name.' }; $names += $guest.vm_name
        Assert-FabricsMatrixText $guest.distribution '^(Ubuntu|RHEL)$' 'distribution'
        Assert-FabricsMatrixText $guest.architecture '^(x86_64|aarch64)$' 'guest architecture'
        Assert-FabricsMatrixText $guest.host_name '^[A-Za-z0-9][A-Za-z0-9.-]*$' 'SSH host'
        Assert-FabricsMatrixText $guest.user_name '^[a-z_][a-z0-9_-]{0,31}$' 'SSH user'
        if ($guest.PSObject.Properties.Name -notcontains 'port') { $guest | Add-Member NoteProperty port 22 }
        Assert-FabricsMatrixInteger $guest.port 1 65535 'SSH port'
        $guest.installer = Resolve-FabricsMatrixPath $guest.installer $base -ExistingFile
        Assert-FabricsMatrixFileHash $guest.installer $guest.installer_sha256
        $guest.known_hosts_file = Resolve-FabricsMatrixPath $guest.known_hosts_file $base -ExistingFile
        if ($guest.PSObject.Properties.Name -contains 'identity_file') { $guest.identity_file = Resolve-FabricsMatrixPath $guest.identity_file $base -ExistingFile }
        Set-FabricsMatrixTestDefaults $guest
        if ($guest.model -like 'gpt-oss-*') { Write-Host "Guest '$($guest.vm_name)' must already have enough RAM/disk for $($guest.model); 6 GiB is inadequate." }
    }
    if (-not $plan.enable_hyperv -and $null -eq $plan.windows -and $plan.provision.Count -eq 0 -and $plan.guests.Count -eq 0) { throw 'Plan contains no operations.' }
    return $plan
}
function Get-FabricsMatrixScriptsHash {
    $inputs = @('Invoke-NativeMatrix.ps1','Enable-HyperV.ps1','New-LinuxTestVM.ps1','Invoke-WindowsHostTest.ps1','Invoke-LinuxGuestTest.ps1','Test-Common.ps1','guest-test.sh','../smoke.py')
    $lines = foreach ($relative in $inputs) { $relative + ':' + (Get-FileHash -LiteralPath (Join-Path $matrixScriptRoot $relative) -Algorithm SHA256).Hash.ToLowerInvariant() }
    $algorithm = [Security.Cryptography.SHA256]::Create()
    try { return ([BitConverter]::ToString($algorithm.ComputeHash([Text.Encoding]::UTF8.GetBytes(($lines -join "`n"))))).Replace('-','').ToLowerInvariant() }
    finally { $algorithm.Dispose() }
}
function Assert-FabricsMatrixSnapshot {
    param([string]$Path, [string]$PlanSHA, [string]$ScriptsSHA)
    Assert-FabricsMatrixFileHash $Path $PlanSHA
    if ((Get-FabricsMatrixScriptsHash) -ne $ScriptsSHA) { throw 'Native test scripts changed after launch; no next operation will run.' }
}
function Test-FabricsMatrixAdministrator {
    $principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
    return $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}
function Start-FabricsMatrixElevation {
    param([string]$Path, [string]$PlanSHA, [string]$ScriptsSHA)
    $script = Join-Path $matrixScriptRoot 'Invoke-NativeMatrix.ps1'
    $command = "& '" + $script.Replace("'", "''") + "' -PlanPath '" + $Path.Replace("'", "''") + "' -Elevated -ExpectedPlanSHA256 '" + $PlanSHA + "' -ExpectedScriptsSHA256 '" + $ScriptsSHA + "'"
    $encoded = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($command))
    $powershell = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
    $process = Start-Process -FilePath $powershell -Verb RunAs -ArgumentList @('-NoLogo','-NoProfile','-EncodedCommand',$encoded) -WorkingDirectory $matrixScriptRoot -Wait -PassThru
    return [int]$process.ExitCode
}
function Invoke-FabricsMatrixChild {
    param([string]$Name, [hashtable]$Parameters)
    & (Join-Path $matrixScriptRoot $Name) @Parameters
}
function Invoke-FabricsMatrixExecution {
    param($Plan, [string]$Path, [string]$PlanSHA, [string]$ScriptsSHA)
    Assert-FabricsMatrixSnapshot $Path $PlanSHA $ScriptsSHA
    New-Item -ItemType Directory -Path $Plan.report_root -ErrorAction Stop | Out-Null
    $record = [ordered]@{ schema_version = 1; status = 'failed'; last_stage = 'host'; plan_sha256 = $PlanSHA; scripts_sha256 = $ScriptsSHA; report_root = $Plan.report_root; native_tests_completed = 0; completed_targets = @(); failure = $null; operations = @(); scope = 'Windows CPU and verified Hyper-V Linux CPU guests; no macOS or GPU validation'; reboot_required = $false }
    $exitCode = 1
    try {
        $record.host = [ordered]@{
            operating_system = (Get-CimInstance Win32_OperatingSystem | Select-Object Caption,Version,OSArchitecture)
            processors = @(Get-CimInstance Win32_Processor | Select-Object Name,Manufacturer,Architecture,NumberOfCores,NumberOfLogicalProcessors)
            computer = (Get-CimInstance Win32_ComputerSystem | Select-Object TotalPhysicalMemory,NumberOfLogicalProcessors,HypervisorPresent)
        }
        if ($Plan.enable_hyperv -or $Plan.provision.Count -gt 0 -or $Plan.guests.Count -gt 0) {
            $record.last_stage = 'hyperv_readiness'
            $parameters = @{ Confirm = $false }; if ($Plan.enable_hyperv) { $parameters.Enable = $true }
            Assert-FabricsMatrixSnapshot $Path $PlanSHA $ScriptsSHA
            $readiness = (Invoke-FabricsMatrixChild 'Enable-HyperV.ps1' $parameters | Out-String) | ConvertFrom-Json
            $record.hyperv = $readiness
            if ($readiness.reboot_required -or ($Plan.enable_hyperv -and -not $readiness.hypervisor_running)) {
                $record.status = 'blocked_reboot'; $record.reboot_required = $true
                $record.failure = 'Save work and restart Windows manually; this elevated session ends here. A later launch requires its own UAC consent.'
                $exitCode = 2; return $exitCode
            }
            if ($readiness.feature_state -ne 'Enabled' -or -not $readiness.hypervisor_running) { throw 'Hyper-V is not ready. Set enable_hyperv true only when enabling is intended, and complete any required host restart.' }
        }
        foreach ($vm in $Plan.provision) {
            $record.last_stage = 'provision:' + $vm.name
            Assert-FabricsMatrixSnapshot $Path $PlanSHA $ScriptsSHA
            Assert-FabricsMatrixFileHash $vm.iso_path $vm.iso_sha256
            $parameters = @{ Distribution=$vm.distribution; ISOPath=$vm.iso_path; ExpectedSHA256=$vm.iso_sha256; Name=$vm.name; VMRoot=$vm.vm_root; SwitchName=$vm.switch_name; MemoryBytes=[UInt64]$vm.memory_bytes; Processors=[int]$vm.processors; DiskBytes=[UInt64]$vm.disk_bytes; Start=[bool]$vm.start; Confirm=$false }
            $result = (Invoke-FabricsMatrixChild 'New-LinuxTestVM.ps1' $parameters | Out-String) | ConvertFrom-Json
            $record.operations += @{ stage=$record.last_stage; result=$result }
        }
        if ($null -ne $Plan.windows) {
            $record.last_stage = 'windows_cpu'
            Assert-FabricsMatrixSnapshot $Path $PlanSHA $ScriptsSHA
            Assert-FabricsMatrixFileHash $Plan.windows.installer $Plan.windows.installer_sha256
            $parameters = @{ Installer=$Plan.windows.installer; InstallerSHA256=$Plan.windows.installer_sha256; Backend='cpu'; Model=$Plan.windows.model; TurnTimeoutSeconds=[int]$Plan.windows.turn_timeout_seconds; ReportDirectory=(Join-Path $Plan.report_root 'windows-cpu') }
            Invoke-FabricsMatrixChild 'Invoke-WindowsHostTest.ps1' $parameters | Out-Null
            $record.native_tests_completed++
            $record.completed_targets += @{ platform='windows'; backend='cpu'; model=$Plan.windows.model; report_directory=$parameters.ReportDirectory }
        }
        foreach ($guest in $Plan.guests) {
            $record.last_stage = 'guest:' + $guest.vm_name
            Assert-FabricsMatrixSnapshot $Path $PlanSHA $ScriptsSHA
            Assert-FabricsMatrixFileHash $guest.installer $guest.installer_sha256
            $parameters = @{ VMName=$guest.vm_name; ExpectedDistribution=$guest.distribution; ExpectedArchitecture=$guest.architecture; HostName=$guest.host_name; UserName=$guest.user_name; Port=[int]$guest.port; KnownHostsFile=$guest.known_hosts_file; Installer=$guest.installer; InstallerSHA256=$guest.installer_sha256; Model=$guest.model; TurnTimeoutSeconds=[int]$guest.turn_timeout_seconds; ReportDirectory=(Join-Path $Plan.report_root $guest.vm_name) }
            if ($guest.identity_file) { $parameters.IdentityFile = $guest.identity_file }
            Invoke-FabricsMatrixChild 'Invoke-LinuxGuestTest.ps1' $parameters | Out-Null
            $record.native_tests_completed++
            $record.completed_targets += @{ platform='linux'; distribution=$guest.distribution; architecture=$guest.architecture; vm_name=$guest.vm_name; backend='cpu'; model=$guest.model; report_directory=$parameters.ReportDirectory }
        }
        $record.last_stage = 'complete'
        $record.status = $(if ($record.native_tests_completed -gt 0) { 'passed' } else { 'prepared' })
        if ($Plan.provision.Count -gt 0 -and $Plan.guests.Count -eq 0) { $record.next_step = 'Install each guest OS interactively, prepare Python/SSH, verify host fingerprints, then create a new plan for guest CPU tests.' }
        $exitCode = 0
    }
    catch {
        $record.failure = $_.Exception.Message
        if ($record.last_stage -like 'guest:*' -and -not (Test-Path -LiteralPath (Join-Path (Join-Path $Plan.report_root ($record.last_stage.Substring(6))) 'evidence\smoke.json'))) { $record.status = 'blocked_guest_setup' }
        Write-Warning $record.failure
    }
    finally {
        $record | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath (Join-Path $Plan.report_root 'matrix-report.json') -Encoding UTF8
        Write-Host "Matrix status: $($record.status). Evidence preserved: $($Plan.report_root)"
    }
    return $exitCode
}
function Invoke-FabricsNativeMatrix {
    param([string]$Path, [switch]$Validate, [switch]$IsElevated, [string]$PlanSHA, [string]$ScriptsSHA)
    $resolved = (Get-Item -LiteralPath $Path -ErrorAction Stop).FullName
    $currentPlanSHA = (Get-FileHash -LiteralPath $resolved -Algorithm SHA256).Hash.ToLowerInvariant()
    $currentScriptsSHA = Get-FabricsMatrixScriptsHash
    if ($PlanSHA -and $PlanSHA -ne $currentPlanSHA) { throw 'Plan changed after launch; refusing execution.' }
    if ($ScriptsSHA -and $ScriptsSHA -ne $currentScriptsSHA) { throw 'Scripts changed after launch; refusing execution.' }
    if ($IsElevated -and (-not $PlanSHA -or -not $ScriptsSHA)) { throw 'Elevated entry requires the original plan and script hashes.' }
    $plan = Read-FabricsMatrixPlan $resolved
    Write-Host ($plan | ConvertTo-Json -Depth 10)
    Write-Host 'Only the printed plan will run. No service, scheduled task, automatic reboot, VM deletion, macOS testing, or GPU passthrough is configured.'
    if ($Validate) { Write-Host 'Validation passed; no UAC or host changes.'; return 0 }
    if ($env:OS -ne 'Windows_NT') { throw 'Execution requires the actual Windows desktop; ValidateOnly is available elsewhere.' }
    if (-not (Test-FabricsMatrixAdministrator)) {
        if ($IsElevated) { throw 'Administrator rights were not granted; no host changes were made.' }
        return (Start-FabricsMatrixElevation $resolved $currentPlanSHA $currentScriptsSHA)
    }
    return (Invoke-FabricsMatrixExecution $plan $resolved $currentPlanSHA $currentScriptsSHA)
}
$code = Invoke-FabricsNativeMatrix -Path $PlanPath -Validate:$ValidateOnly -IsElevated:$Elevated -PlanSHA $ExpectedPlanSHA256 -ScriptsSHA $ExpectedScriptsSHA256
if (-not $ValidateOnly) { exit $code }
