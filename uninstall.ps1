Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

if ($env:OS -ne "Windows_NT") {
    throw "cpdo: uninstall.ps1 only supports Windows"
}

$cpdoInstallDir = Join-Path ([Environment]::GetFolderPath("LocalApplicationData")) "Programs\cpdo"
$cpdoBinary = Join-Path $cpdoInstallDir "cpdo.exe"
if (Test-Path -LiteralPath $cpdoBinary) {
    Remove-Item -LiteralPath $cpdoBinary -Force
    Write-Host "Removed $cpdoBinary"
    Remove-Item -LiteralPath $cpdoInstallDir -Force -ErrorAction SilentlyContinue
}
else {
    Write-Host "cpdo is not installed at $cpdoBinary"
}

$cpdoConfig = Join-Path ([Environment]::GetFolderPath("UserProfile")) ".config\cpdo\config.json"
Write-Host "Kept configuration at $cpdoConfig"
