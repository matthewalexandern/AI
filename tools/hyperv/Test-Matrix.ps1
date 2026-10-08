#Requires -Version 5.1
# Protocol fixtures only: no UAC, Windows feature, VM, SSH, or installer runs.
[CmdletBinding()]
param()
$ErrorActionPreference = 'Stop'
$fixture = Join-Path ([IO.Path]::GetTempPath()) ('mini-fabrics-matrix-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $fixture | Out-Null
$savedOS = $env:OS
$savedSystemRoot = $env:SystemRoot
try {
    $installer = Join-Path $fixture 'installer'
    Set-Content -LiteralPath $installer -Value 'Fixture only; never execute.'
    $digest = (Get-FileHash -LiteralPath $installer -Algorithm SHA256).Hash
    $knownHosts = Join-Path $fixture 'known_hosts'
    Set-Content -LiteralPath $knownHosts -Value 'Fixture trust store only.'
    $iso = Join-Path $fixture 'trusted.iso'
    Set-Content -LiteralPath $iso -Value 'Fixture ISO only.'
    $isoDigest = (Get-FileHash -LiteralPath $iso -Algorithm SHA256).Hash
    $index = 0
    function New-MatrixFixturePlan {
        $script:index++
        return [ordered]@{ schema_version=1; enable_hyperv=$false; report_root=(Join-Path $fixture "report-$script:index"); windows=@{ installer=$installer; installer_sha256=$digest }; provision=@(); guests=@() }
    }
    function Save-MatrixFixturePlan {
        param($Value)
        $path = Join-Path $fixture ([guid]::NewGuid().ToString('N') + '.json')
        $Value | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $path -Encoding UTF8
        return $path
    }
    function Assert-MatrixFixtureRejected {
        param($Value)
        $rejected = $false
        try { $null = Read-FabricsMatrixPlan (Save-MatrixFixturePlan $Value) }
        catch { $rejected = $true }
        if (-not $rejected -or (Test-Path -LiteralPath $Value.report_root)) { throw 'Invalid matrix plan was accepted or created evidence.' }
    }
    $initial = Save-MatrixFixturePlan (New-MatrixFixturePlan)
    . (Join-Path $PSScriptRoot 'Invoke-NativeMatrix.ps1') -PlanPath $initial -ValidateOnly 6>$null
    # Normal JSON editors produce BOM-less UTF-8. Windows PowerShell 5.1 must
    # preserve non-ASCII paths rather than decoding them as system ANSI.
    $utf8Plan = New-MatrixFixturePlan
    $utf8Plan.report_root = Join-Path $fixture ('report-' + [char]0xe9)
    $utf8Path = Join-Path $fixture 'utf8-plan.json'
    [IO.File]::WriteAllText($utf8Path, ($utf8Plan | ConvertTo-Json -Depth 12), (New-Object Text.UTF8Encoding($false)))
    $utf8Read = Read-FabricsMatrixPlan $utf8Path
    if ($utf8Read.report_root -cne $utf8Plan.report_root) { throw 'BOM-less UTF-8 plan paths were corrupted.' }
    $originalElevation = ${function:Start-FabricsMatrixElevation}
    $script:matrixFixtureLaunches = @()
    function Start-Process {
        param([string]$FilePath,[string]$Verb,[string[]]$ArgumentList,[string]$WorkingDirectory,[switch]$Wait,[switch]$PassThru)
        $script:matrixFixtureLaunches += @{ file=$FilePath; verb=$Verb; arguments=$ArgumentList; wait=[bool]$Wait; pass_thru=[bool]$PassThru }
        return [pscustomobject]@{ ExitCode=0 }
    }
    # Windows cryptographic providers depend on the real SystemRoot. Only
    # synthesize a path for the mocked launcher on a non-Windows test host.
    if ([string]::IsNullOrEmpty($savedSystemRoot)) {
        if ($savedOS -eq 'Windows_NT') { throw 'Windows fixture requires the actual SystemRoot.' }
        $env:SystemRoot=$fixture
    }
    $expectedPowerShell = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
    $quotedPath=Join-Path $fixture "plan's quoted name.json"
    Copy-Item -LiteralPath $initial -Destination $quotedPath
    $null=& $originalElevation -Path $quotedPath -PlanSHA $digest -ScriptsSHA $digest
    $launch=$script:matrixFixtureLaunches[0]
    if ($launch.file -cne $expectedPowerShell -or ($savedSystemRoot -and $env:SystemRoot -cne $savedSystemRoot)) { throw 'Launcher fixture changed the OS root or selected an unexpected Windows PowerShell path.' }
    $env:SystemRoot=$savedSystemRoot
    $decoded=[Text.Encoding]::Unicode.GetString([Convert]::FromBase64String($launch.arguments[-1]))
    $tokens=$null; $errors=$null
    $null=[System.Management.Automation.Language.Parser]::ParseInput($decoded,[ref]$tokens,[ref]$errors)
    if ($script:matrixFixtureLaunches.Count -ne 1 -or $launch.verb -ne 'RunAs' -or -not $launch.wait -or -not $launch.pass_thru -or $errors.Count -ne 0 -or $decoded -notlike "*plan''s quoted name.json*" -or $launch.arguments -contains '-ExecutionPolicy') { throw 'Single-UAC encoded launch or safe path quoting failed.' }
    $script:matrixFixtureTrace = @()
    $script:matrixFixtureElevations = 0
    $script:matrixFixtureAdmin = $false
    $script:matrixFixtureReboot = $false
    $script:matrixFixtureFailure = $false
    function Test-FabricsMatrixAdministrator { return $script:matrixFixtureAdmin }
    function Start-FabricsMatrixElevation {
        param([string]$Path,[string]$PlanSHA,[string]$ScriptsSHA)
        $script:matrixFixtureElevations++
        Assert-FabricsMatrixSnapshot $Path $PlanSHA $ScriptsSHA
        return 0
    }
    function Get-CimInstance { param([string]$ClassName) return [pscustomobject]@{ Caption='Fixture Windows'; Version='Fixture'; OSArchitecture='64-bit' } }
    function Invoke-FabricsMatrixChild {
        param([string]$Name,[hashtable]$Parameters)
        $script:matrixFixtureTrace += @{ name=$Name; parameters=$Parameters }
        if ($Name -eq 'Enable-HyperV.ps1') {
            return (@{ feature_state='Enabled'; hypervisor_running=$true; reboot_required=$script:matrixFixtureReboot } | ConvertTo-Json)
        }
        if ($Name -eq 'New-LinuxTestVM.ps1') { return (@{ status='created'; name=$Parameters.Name } | ConvertTo-Json) }
        New-Item -ItemType Directory -Path $Parameters.ReportDirectory | Out-Null
        Set-Content -LiteralPath (Join-Path $Parameters.ReportDirectory 'retained.log') -Value 'Fixture evidence survives failure.'
        if ($script:matrixFixtureFailure) { throw 'Fixture child failed before inference.' }
    }
    $env:OS = 'Windows_NT'
    # Closed schema, booleans, bounds, checksums, and command injection fields.
    $bad = New-MatrixFixturePlan; $bad.command='Write-Host forbidden'; Assert-MatrixFixtureRejected $bad
    $bad = New-MatrixFixturePlan; $bad.enable_hyperv='true'; Assert-MatrixFixtureRejected $bad
    $bad = New-MatrixFixturePlan; $bad.schema_version=2; Assert-MatrixFixtureRejected $bad
    $bad = New-MatrixFixturePlan; $bad.windows.installer_sha256=('0' * 64); Assert-MatrixFixtureRejected $bad
    $bad = New-MatrixFixturePlan; $bad.windows.turn_timeout_seconds=3601; Assert-MatrixFixtureRejected $bad
    $bad = New-MatrixFixturePlan; $bad.windows.python='powershell -Command arbitrary'; Assert-MatrixFixtureRejected $bad
    $bad = New-MatrixFixturePlan; $bad.windows=$null; Assert-MatrixFixtureRejected $bad
    $bad = New-MatrixFixturePlan; $bad.provision='not-an-array'; Assert-MatrixFixtureRejected $bad
    # ValidateOnly never elevates; a non-admin execution elevates exactly once.
    $path = Save-MatrixFixturePlan (New-MatrixFixturePlan)
    $null = Invoke-FabricsNativeMatrix -Path $path -Validate 6>$null
    if ($script:matrixFixtureElevations -ne 0 -or $script:matrixFixtureTrace.Count -ne 0) { throw 'ValidateOnly caused execution.' }
    $code = Invoke-FabricsNativeMatrix -Path $path 6>$null
    if ($code -ne 0 -or $script:matrixFixtureElevations -ne 1 -or $script:matrixFixtureTrace.Count -ne 0) { throw 'Non-admin launch did not use exactly one elevation.' }
    # Existing administrator token is reused without another elevation.
    $script:matrixFixtureAdmin = $true
    $code = Invoke-FabricsNativeMatrix -Path $path 6>$null
    if ($code -ne 0 -or $script:matrixFixtureElevations -ne 1 -or $script:matrixFixtureTrace.Count -ne 1) { throw 'Admin reuse or child execution failed.' }
    $windows = $script:matrixFixtureTrace[0].parameters
    if ($windows.Backend -ne 'cpu' -or $windows.Model -ne 'gpt-oss-20b' -or $windows.TurnTimeoutSeconds -ne 1800) { throw 'Windows defaults were not forwarded.' }
    $report=Get-Content -LiteralPath (Join-Path (Split-Path -Parent $windows.ReportDirectory) 'matrix-report.json') -Raw | ConvertFrom-Json
    if ($report.completed_targets.Count -ne 1 -or $report.completed_targets[0].platform -ne 'windows') { throw 'Completed targets overstate Windows-only evidence.' }
    # A report directory is preserved on rerun rather than replaced.
    $rejected = $false; try { $null = Invoke-FabricsNativeMatrix -Path $path 6>$null } catch { $rejected=$true }
    if (-not $rejected) { throw 'Existing report directory was overwritten.' }
    # Pending reboot halts before installers, VMs, or guest SSH calls.
    $script:matrixFixtureTrace=@(); $script:matrixFixtureReboot=$true
    $plan=New-MatrixFixturePlan; $plan.enable_hyperv=$true; $path=Save-MatrixFixturePlan $plan
    $code=Invoke-FabricsNativeMatrix -Path $path 6>$null
    $report=Get-Content -LiteralPath (Join-Path $plan.report_root 'matrix-report.json') -Raw | ConvertFrom-Json
    if ($code -eq 0 -or $script:matrixFixtureTrace.Count -ne 1 -or $report.status -ne 'blocked_reboot' -or $report.native_tests_completed -ne 0) { throw 'Reboot did not stop the approved plan.' }
    if ($script:matrixFixtureTrace[0].parameters.Confirm -ne $false) { throw 'Child confirmation suppression was lost.' }
    # Provision and sequential guest calls carry only fixed, validated fields.
    $script:matrixFixtureTrace=@(); $script:matrixFixtureReboot=$false
    $plan=New-MatrixFixturePlan
    $plan.provision=@(@{ distribution='Ubuntu'; iso_path=$iso; iso_sha256=$isoDigest; name='Fixture-Ubuntu'; vm_root=(Join-Path $fixture 'vms'); switch_name='Default Switch'; memory_bytes=24GB; processors=2; disk_bytes=48GB; start=$false })
    foreach ($distribution in @('Ubuntu','RHEL')) {
        $plan.guests += @{ vm_name=('Fixture-' + $distribution); distribution=$distribution; architecture='x86_64'; host_name='192.168.1.120'; user_name='tester'; known_hosts_file=$knownHosts; installer=$installer; installer_sha256=$digest; model='qwen2.5-1.5b'; turn_timeout_seconds=900 }
    }
    $path=Save-MatrixFixturePlan $plan; $code=Invoke-FabricsNativeMatrix -Path $path 6>$null
    if ($code -ne 0 -or $script:matrixFixtureTrace.Count -ne 5) { throw 'Native matrix did not execute its fixed sequence.' }
    $provision=$script:matrixFixtureTrace[1].parameters
    if ($provision.Confirm -ne $false -or $provision.Start -ne $false -or $provision.MemoryBytes -ne 24GB) { throw 'Provisioning fields changed.' }
    foreach ($entry in $script:matrixFixtureTrace[3..4]) {
        $p=$entry.parameters
        if ($p.KnownHostsFile -ne $knownHosts -or $p.Port -ne 22 -or $p.Model -ne 'qwen2.5-1.5b' -or $p.TurnTimeoutSeconds -ne 900 -or $p.ExpectedArchitecture -ne 'x86_64' -or $p.VMName -notlike 'Fixture-*') { throw 'Guest binding/trust/deadline fields were not forwarded.' }
    }
    $report=Get-Content -LiteralPath (Join-Path $plan.report_root 'matrix-report.json') -Raw | ConvertFrom-Json
    if ($report.completed_targets.Count -ne 3 -or @($report.completed_targets | Where-Object { $_.platform -eq 'linux' }).Count -ne 2) { throw 'Completed native targets were not recorded accurately.' }
    # Failed child evidence and the complete matrix result remain available.
    $script:matrixFixtureFailure=$true; $script:matrixFixtureTrace=@()
    $plan=New-MatrixFixturePlan; $path=Save-MatrixFixturePlan $plan
    $code=Invoke-FabricsNativeMatrix -Path $path 6>$null 3>$null
    $report=Get-Content -LiteralPath (Join-Path $plan.report_root 'matrix-report.json') -Raw | ConvertFrom-Json
    if ($code -eq 0 -or $report.status -ne 'failed' -or -not (Test-Path -LiteralPath (Join-Path $plan.report_root 'windows-cpu\retained.log'))) { throw 'Failure was hidden or evidence was removed.' }
    # Installers modified after preflight cannot execute.
    $script:matrixFixtureTrace=@(); $script:matrixFixtureFailure=$false
    $plan=New-MatrixFixturePlan; $path=Save-MatrixFixturePlan $plan
    $validated=Read-FabricsMatrixPlan $path
    $planSHA=(Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash
    Add-Content -LiteralPath $installer -Value 'Modified after validation.'
    $code=Invoke-FabricsMatrixExecution $validated $path $planSHA (Get-FabricsMatrixScriptsHash) 6>$null 3>$null
    if ($code -eq 0 -or $script:matrixFixtureTrace.Count -ne 0) { throw 'Changed installer was executed.' }
    Set-Content -LiteralPath $installer -Value 'Fixture only; never execute.'
    # Elevated entry requires original pins and rejects changed plan bytes.
    $plan=New-MatrixFixturePlan; $path=Save-MatrixFixturePlan $plan
    $rejected=$false; try { $null=Invoke-FabricsNativeMatrix -Path $path -IsElevated 6>$null } catch { $rejected=$true }
    if (-not $rejected) { throw 'Unpinned elevated entry was accepted.' }
    $original=(Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash
    Add-Content -LiteralPath $path -Value ' '
    $rejected=$false; try { $null=Invoke-FabricsNativeMatrix -Path $path -IsElevated -PlanSHA $original -ScriptsSHA (Get-FabricsMatrixScriptsHash) 6>$null } catch { $rejected=$true }
    if (-not $rejected -or (Test-Path -LiteralPath $plan.report_root)) { throw 'Mutated elevated plan was executed.' }
    Write-Host 'Native matrix fixtures passed: closed schema, UTF-8 paths, one elevation, admin reuse, reboot stop, fixed forwarding, pinned inputs, and retained failure evidence. No UAC, Hyper-V, SSH, or native inference executed.'
}
finally {
$env:OS=$savedOS
    $env:SystemRoot=$savedSystemRoot
    Remove-Item -LiteralPath $fixture -Recurse -Force
}
