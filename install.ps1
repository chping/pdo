Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

if ($env:OS -ne "Windows_NT") {
    throw "pdo: install.ps1 only supports Windows"
}

$pdoProcessor = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
$pdoArch = switch ($pdoProcessor.ToUpperInvariant()) {
    "AMD64" { "amd64" }
    "ARM64" { "arm64" }
    default { throw "pdo: unsupported architecture: $pdoProcessor" }
}

$pdoAsset = "pdo_windows_$pdoArch.exe"
$pdoBaseUrl = "https://github.com/chping/pdo/releases/latest/download"
$pdoTempDir = Join-Path ([IO.Path]::GetTempPath()) ("pdo-" + [Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $pdoTempDir | Out-Null

try {
    $pdoDownload = Join-Path $pdoTempDir $pdoAsset
    $pdoChecksums = Join-Path $pdoTempDir "SHA256SUMS"
    Invoke-WebRequest -UseBasicParsing -Uri "$pdoBaseUrl/$pdoAsset" -OutFile $pdoDownload
    Invoke-WebRequest -UseBasicParsing -Uri "$pdoBaseUrl/SHA256SUMS" -OutFile $pdoChecksums

    $pdoExpected = $null
    foreach ($pdoLine in Get-Content -LiteralPath $pdoChecksums) {
        $pdoParts = $pdoLine.Trim() -split '\s+', 2
        if ($pdoParts.Count -eq 2 -and $pdoParts[1].TrimStart("*") -eq $pdoAsset) {
            $pdoExpected = $pdoParts[0].ToLowerInvariant()
            break
        }
    }
    if (-not $pdoExpected) {
        throw "pdo: checksum not found for $pdoAsset"
    }
    $pdoActual = (Get-FileHash -Algorithm SHA256 -LiteralPath $pdoDownload).Hash.ToLowerInvariant()
    if ($pdoExpected -ne $pdoActual) {
        throw "pdo: checksum verification failed"
    }

    $pdoInstallDir = Join-Path ([Environment]::GetFolderPath("LocalApplicationData")) "Programs\pdo"
    New-Item -ItemType Directory -Force -Path $pdoInstallDir | Out-Null
    $pdoStaged = Join-Path $pdoInstallDir (".pdo-" + $PID + ".tmp")
    Copy-Item -LiteralPath $pdoDownload -Destination $pdoStaged -Force
    Move-Item -LiteralPath $pdoStaged -Destination (Join-Path $pdoInstallDir "pdo.exe") -Force

    $pdoConfigDir = Join-Path ([Environment]::GetFolderPath("UserProfile")) ".config\pdo"
    $pdoConfig = Join-Path $pdoConfigDir "config.json"
    if (-not (Test-Path -LiteralPath $pdoConfig)) {
        New-Item -ItemType Directory -Force -Path $pdoConfigDir | Out-Null
        $pdoTemplate = @'
{
  "schema_version": 1,
  "github": {
    "pat_env": "PDO_GITHUB_PAT"
  },
  "dotfiles": {
    "ssh-config": {
      "remote": "https://api.github.com/repos/OWNER/REPOSITORY/contents/ssh_config?ref=main",
      "local": "~/.ssh/config"
    }
  }
}
'@
        $pdoUtf8 = New-Object System.Text.UTF8Encoding($false)
        [IO.File]::WriteAllText($pdoConfig, $pdoTemplate + [Environment]::NewLine, $pdoUtf8)
        Write-Host "Created $pdoConfig"
    }

    Write-Host "Installed pdo to $pdoInstallDir\pdo.exe"
    $pdoUserPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if (($pdoUserPath -split ";") -notcontains $pdoInstallDir) {
        Write-Warning "$pdoInstallDir is not in PATH. Add it with:"
        Write-Host "[Environment]::SetEnvironmentVariable('Path', [Environment]::GetEnvironmentVariable('Path', 'User') + ';$pdoInstallDir', 'User')"
    }
    Write-Host "Edit $pdoConfig and set `$env:PDO_GITHUB_PAT in your environment."
}
finally {
    Remove-Item -LiteralPath $pdoTempDir -Recurse -Force -ErrorAction SilentlyContinue
}
