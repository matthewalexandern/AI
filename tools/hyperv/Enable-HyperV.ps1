#Requires -Version 5.1
<#
.SYNOPSIS
Report desktop Hyper-V readiness, or enable it only when -Enable is provided.
.DESCRIPTION
Run in elevated PowerShell on a supported Windows desktop. Enabling never
restarts the host. Inspect reboot_required and restart manually when convenient.
#>
[CmdletBinding(SupportsShouldProcess = $true, ConfirmImpact = 'Medium')]
param([switch]$Enable)

$ErrorActionPreference = 'Stop'
if ($env:OS -ne 'Windows_NT') { throw 'This script requires Windows desktop.' }
$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Open PowerShell as Administrator to inspect or enable the Windows Hyper-V feature.'
}
$operatingSystem = Get-CimInstance Win32_OperatingSystem
if ($operatingSystem.ProductType -ne 1) { throw 'This harness targets Windows desktop; configure Windows Server Hyper-V separately.' }
$edition = (Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion').EditionID
if ($edition -notmatch '^(Professional|Enterprise|Education|IoTEnterprise)') {
    throw "Windows edition '$edition' does not support this desktop Hyper-V workflow. Use Pro, Enterprise, or Education."
}
$feature = Get-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V-All
$computer = Get-CimInstance Win32_ComputerSystem
$processors = @(Get-CimInstance Win32_Processor)
$hardwareReady = [bool]$computer.HypervisorPresent -or (@($processors | Where-Object {
    -not $_.VMMonitorModeExtensions -or -not $_.SecondLevelAddressTranslationExtensions -or -not $_.VirtualizationFirmwareEnabled
}).Count -eq 0 -and $processors.Count -gt 0)
$restartRequired = "$($feature.State)" -eq 'EnablePending'
$changed = $false
if ($Enable -and "$($feature.State)" -notin @('Enabled', 'EnablePending')) {
    if (-not $hardwareReady) {
        throw 'Enable CPU virtualization in UEFI/BIOS and confirm SLAT support before enabling Hyper-V.'
    }
    if ($PSCmdlet.ShouldProcess('Windows desktop', 'Enable Microsoft-Hyper-V-All without restarting')) {
        $result = Enable-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V-All -All -NoRestart
        $changed = $true
        $restartRequired = [bool]$result.RestartNeeded
        $feature = Get-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V-All
        $restartRequired = $restartRequired -or "$($feature.State)" -eq 'EnablePending'
    }
}
[ordered]@{
    edition = $edition
    feature_state = "$($feature.State)"
    hardware_ready = $hardwareReady
    hypervisor_running = [bool]$computer.HypervisorPresent
    changed = $changed
    reboot_required = $restartRequired
    next_step = $(if ($restartRequired) { 'Restart the host manually before creating VMs.' }
                  elseif ("$($feature.State)" -ne 'Enabled') { 'Rerun with -Enable to opt in to enabling Hyper-V.' }
                  elseif (-not $computer.HypervisorPresent) { 'Hyper-V is installed but the hypervisor is not running; check the boot configuration and restart manually.' }
                  else { 'Ready for New-LinuxTestVM.ps1.' })
} | ConvertTo-Json
