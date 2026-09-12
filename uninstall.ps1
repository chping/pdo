Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

if ($env:OS -ne "Windows_NT") {
    throw "pdo: uninstall.ps1 only supports Windows"
}

$pdoInstallDir = Join-Path ([Environment]::GetFolderPath("LocalApplicationData")) "Programs\pdo"
$pdoBinary = Join-Path $pdoInstallDir "pdo.exe"
if (Test-Path -LiteralPath $pdoBinary) {
    Remove-Item -LiteralPath $pdoBinary -Force
    Write-Host "Removed $pdoBinary"
    Remove-Item -LiteralPath $pdoInstallDir -Force -ErrorAction SilentlyContinue
}
else {
    Write-Host "pdo is not installed at $pdoBinary"
}

$pdoConfig = Join-Path ([Environment]::GetFolderPath("UserProfile")) ".config\pdo\config.json"
Write-Host "Kept configuration at $pdoConfig"
