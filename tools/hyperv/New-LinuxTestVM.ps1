#Requires -Version 5.1
<#
.SYNOPSIS
Create a CPU-only Generation 2 Ubuntu or RHEL test VM from a verified local ISO.
.DESCRIPTION
No OS unattended installation, automatic cleanup, checkpoints, or GPU assignment.
Existing VMs and virtual disks are preserved. The selected virtual switch must
already exist. Use -WhatIf to preview creation after prerequisite verification.
#>
[CmdletBinding(SupportsShouldProcess = $true, ConfirmImpact = 'Medium')]
param(
    [Parameter(Mandatory = $true)][ValidateSet('Ubuntu', 'RHEL')][string]$Distribution,
    [Parameter(Mandatory = $true)][string]$ISOPath,
    [Parameter(Mandatory = $true)][ValidatePattern('^[a-fA-F0-9]{64}$')][string]$ExpectedSHA256,
    [ValidatePattern('^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$')][string]$Name,
    [string]$VMRoot = (Join-Path $env:PUBLIC 'Documents\MiniFabricsHyperV'),
    [string]$SwitchName = 'Default Switch',
    [ValidateRange(2GB, 128GB)][UInt64]$MemoryBytes = 6GB,
    [ValidateRange(1, 32)][int]$Processors = 2,
    [ValidateRange(32GB, 512GB)][UInt64]$DiskBytes = 48GB,
    [switch]$Start
)

$ErrorActionPreference = 'Stop'
if ($env:OS -ne 'Windows_NT') { throw 'This script requires Windows desktop with Hyper-V.' }
$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Open PowerShell as Administrator before creating a Hyper-V VM.'
}
$feature = Get-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V-All
if ("$($feature.State)" -ne 'Enabled') {
    throw 'Hyper-V is not enabled and ready. Run Enable-HyperV.ps1, opt in with -Enable, and complete any required restart.'
}
$computer = Get-CimInstance Win32_ComputerSystem
if (-not $computer.HypervisorPresent) { throw 'The Hyper-V hypervisor is not running. Complete any required host restart first.' }
Import-Module Hyper-V -ErrorAction Stop
if (-not $Name) { $Name = 'MiniFabrics-' + $Distribution }
$iso = Get-Item -LiteralPath $ISOPath
if ($iso.PSIsContainer -or $iso.Extension -ne '.iso') { throw 'ISOPath must name an existing local .iso file.' }
if ((Get-FileHash -LiteralPath $iso.FullName -Algorithm SHA256).Hash -ne $ExpectedSHA256) {
    throw 'ISO SHA256 does not match the independently obtained publisher checksum; no VM changes were made.'
}
$virtualSwitch = Get-VMSwitch -Name $SwitchName -ErrorAction Stop
$existing = Get-VM -Name $Name -ErrorAction SilentlyContinue
if ($existing) {
    if ($Start -and $existing.State -eq 'Off' -and $PSCmdlet.ShouldProcess($Name, 'Start existing VM without modifying its configuration')) {
        Start-VM -VM $existing | Out-Null
    }
    [ordered]@{ name = $Name; status = 'existing_vm_preserved'; state = "$( (Get-VM -Name $Name).State )"; next_step = 'Existing disks, firmware, ISO attachments, and network settings were preserved. Inspect this VM before using it.' } | ConvertTo-Json
    return
}
$resolvedRoot = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($VMRoot)
$vmDirectory = Join-Path $resolvedRoot $Name
$diskPath = Join-Path $vmDirectory ($Name + '.vhdx')
if (Test-Path -LiteralPath $vmDirectory) {
    throw "Preserving existing directory '$vmDirectory'. Choose another -Name/-VMRoot or inspect the existing files manually."
}
if (Test-Path -LiteralPath $diskPath) { throw "Preserving existing virtual disk '$diskPath'." }
$availableBytes = [UInt64](Get-CimInstance Win32_OperatingSystem).FreePhysicalMemory * 1KB
if ($availableBytes -lt ($MemoryBytes + 2GB)) {
    throw 'Insufficient currently available host RAM for the VM plus a 2 GiB host reserve. Lower -MemoryBytes or close applications.'
}
if ($Processors -gt $computer.NumberOfLogicalProcessors) { throw 'Requested VM processors exceed host logical processors.' }
if (-not $PSCmdlet.ShouldProcess($Name, "Create Generation 2 $Distribution CPU VM with verified ISO, $Processors CPUs and $MemoryBytes bytes RAM")) { return }

# Failures leave their new VM/disk in place for inspection; never destroy disks
# or remove an existing VM as an automatic recovery action.
New-Item -ItemType Directory -Path $vmDirectory | Out-Null
$vm = New-VM -Name $Name -Generation 2 -MemoryStartupBytes $MemoryBytes -Path $vmDirectory -NewVHDPath $diskPath -NewVHDSizeBytes $DiskBytes -SwitchName $virtualSwitch.Name
Set-VMProcessor -VM $vm -Count $Processors
Set-VMMemory -VM $vm -DynamicMemoryEnabled $false
Set-VM -VM $vm -AutomaticCheckpointsEnabled $false -CheckpointType Disabled -AutomaticStartAction Nothing -AutomaticStopAction ShutDown
Set-VMFirmware -VM $vm -EnableSecureBoot On -SecureBootTemplate MicrosoftUEFICertificateAuthority
$dvd = Add-VMDvdDrive -VM $vm -Path $iso.FullName -Passthru
Set-VMFirmware -VM $vm -FirstBootDevice $dvd
if ($Start) { Start-VM -VM $vm | Out-Null }
[ordered]@{
    name = $Name
    distribution = $Distribution
    status = 'created'
    state = "$( (Get-VM -Name $Name).State )"
    cpu_only = $true
    vm_directory = $vmDirectory
    iso_sha256 = $ExpectedSHA256.ToLowerInvariant()
    switch_name = $SwitchName
    next_step = 'Open VMConnect, install the OS interactively, and configure SSH key authentication. RHEL requires your own authorized media and subscription where needed.'
} | ConvertTo-Json
