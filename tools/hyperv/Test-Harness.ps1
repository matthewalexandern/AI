#Requires -Version 5.1
# Cross-platform syntax/protocol tests. Does not invoke any Hyper-V operation.
[CmdletBinding()]
param()
$ErrorActionPreference = 'Stop'
foreach ($script in Get-ChildItem -LiteralPath $PSScriptRoot -Filter '*.ps1') {
    $tokens = $null
    $errors = $null
    $null = [System.Management.Automation.Language.Parser]::ParseFile($script.FullName, [ref]$tokens, [ref]$errors)
    if ($errors.Count -ne 0) { throw "PowerShell parse error in $($script.Name): $errors" }
}
. (Join-Path $PSScriptRoot 'Test-Common.ps1')
$testDirectory = Join-Path ([IO.Path]::GetTempPath()) ('mini-fabrics-powershell-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $testDirectory | Out-Null
$previousLocation = Get-Location
try {
    $shell = (Get-Process -Id $PID).Path
    $code = Invoke-FabricsLoggedCommand -Executable $shell -Arguments @('-NoLogo', '-NoProfile', '-Command', 'Write-Output fixture; exit 0') -LogPath (Join-Path $testDirectory 'success.log')
    if ($code -ne 0 -or (Get-Content -LiteralPath (Join-Path $testDirectory 'success.log')) -notcontains 'fixture') { throw 'Success/log protocol failed.' }
    $code = Invoke-FabricsLoggedCommand -Executable $shell -Arguments @('-NoLogo', '-NoProfile', '-Command', 'exit 23') -LogPath (Join-Path $testDirectory 'failure.log')
    if ($code -ne 23) { throw 'Nonzero exit code was not preserved.' }
    $failedAsExpected = $false
    try { $null = Invoke-FabricsLoggedCommand -Executable (Join-Path $testDirectory 'does-not-exist') -Arguments @('unused') -LogPath (Join-Path $testDirectory 'missing.log') }
    catch { $failedAsExpected = $true }
    if (-not $failedAsExpected) { throw 'A missing executable appeared successful.' }
    $failedAsExpected = $false
    try { $null = Invoke-FabricsLoggedCommand -Executable $shell -Arguments @('-NoLogo', '-NoProfile', '-Command', 'Write-Output fixture') -LogPath $testDirectory }
    catch { $failedAsExpected = $true }
    if (-not $failedAsExpected) { throw 'An unwritable log destination appeared successful.' }
    Set-Location -LiteralPath $testDirectory
    $created = New-FabricsEvidenceDirectory -Path 'relative-evidence'
    if ($created -ne (Join-Path $testDirectory 'relative-evidence')) { throw 'Relative evidence path ignored PowerShell current location.' }
    $failedAsExpected = $false
    try { $null = New-FabricsEvidenceDirectory -Path $created }
    catch { $failedAsExpected = $true }
    if (-not $failedAsExpected) { throw 'Existing evidence was not preserved.' }
    & (Join-Path $PSScriptRoot 'Test-NativeArguments.ps1')
    & (Join-Path $PSScriptRoot 'Test-Runners.ps1')
    & (Join-Path $PSScriptRoot 'Test-Matrix.ps1')
    Write-Host 'PowerShell parse and command/evidence protocol tests passed. Hyper-V and Windows native execution were not tested.'
}
finally {
    Set-Location -LiteralPath $previousLocation
    Remove-Item -LiteralPath $testDirectory -Recurse -Force
}
