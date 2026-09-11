Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

if ($env:OS -ne "Windows_NT") {
    throw "cpdo: install.ps1 only supports Windows"
}

$cpdoProcessor = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
$cpdoArch = switch ($cpdoProcessor.ToUpperInvariant()) {
    "AMD64" { "amd64" }
    "ARM64" { "arm64" }
    default { throw "cpdo: unsupported architecture: $cpdoProcessor" }
}

$cpdoAsset = "cpdo_windows_$cpdoArch.exe"
$cpdoBaseUrl = "https://github.com/chping/cpdo/releases/latest/download"
$cpdoTempDir = Join-Path ([IO.Path]::GetTempPath()) ("cpdo-" + [Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $cpdoTempDir | Out-Null

try {
    $cpdoDownload = Join-Path $cpdoTempDir $cpdoAsset
    $cpdoChecksums = Join-Path $cpdoTempDir "SHA256SUMS"
    Invoke-WebRequest -UseBasicParsing -Uri "$cpdoBaseUrl/$cpdoAsset" -OutFile $cpdoDownload
    Invoke-WebRequest -UseBasicParsing -Uri "$cpdoBaseUrl/SHA256SUMS" -OutFile $cpdoChecksums

    $cpdoExpected = $null
    foreach ($cpdoLine in Get-Content -LiteralPath $cpdoChecksums) {
        $cpdoParts = $cpdoLine.Trim() -split '\s+', 2
        if ($cpdoParts.Count -eq 2 -and $cpdoParts[1].TrimStart("*") -eq $cpdoAsset) {
            $cpdoExpected = $cpdoParts[0].ToLowerInvariant()
            break
        }
    }
    if (-not $cpdoExpected) {
        throw "cpdo: checksum not found for $cpdoAsset"
    }
    $cpdoActual = (Get-FileHash -Algorithm SHA256 -LiteralPath $cpdoDownload).Hash.ToLowerInvariant()
    if ($cpdoExpected -ne $cpdoActual) {
        throw "cpdo: checksum verification failed"
    }

    $cpdoInstallDir = Join-Path ([Environment]::GetFolderPath("LocalApplicationData")) "Programs\cpdo"
    New-Item -ItemType Directory -Force -Path $cpdoInstallDir | Out-Null
    $cpdoStaged = Join-Path $cpdoInstallDir (".cpdo-" + $PID + ".tmp")
    Copy-Item -LiteralPath $cpdoDownload -Destination $cpdoStaged -Force
    Move-Item -LiteralPath $cpdoStaged -Destination (Join-Path $cpdoInstallDir "cpdo.exe") -Force

    $cpdoConfigDir = Join-Path ([Environment]::GetFolderPath("UserProfile")) ".config\cpdo"
    $cpdoConfig = Join-Path $cpdoConfigDir "config.json"
    if (-not (Test-Path -LiteralPath $cpdoConfig)) {
        New-Item -ItemType Directory -Force -Path $cpdoConfigDir | Out-Null
        $cpdoTemplate = @'
{
  "schema_version": 1,
  "github": {
    "pat_env": "CPDO_GITHUB_PAT"
  },
  "ssh_config": {
    "repository": "OWNER/REPOSITORY",
    "branch": "main",
    "path": "ssh_config"
  }
}
'@
        $cpdoUtf8 = New-Object System.Text.UTF8Encoding($false)
        [IO.File]::WriteAllText($cpdoConfig, $cpdoTemplate + [Environment]::NewLine, $cpdoUtf8)
        Write-Host "Created $cpdoConfig"
    }

    Write-Host "Installed cpdo to $cpdoInstallDir\cpdo.exe"
    $cpdoUserPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if (($cpdoUserPath -split ";") -notcontains $cpdoInstallDir) {
        Write-Warning "$cpdoInstallDir is not in PATH. Add it with:"
        Write-Host "[Environment]::SetEnvironmentVariable('Path', [Environment]::GetEnvironmentVariable('Path', 'User') + ';$cpdoInstallDir', 'User')"
    }
    Write-Host "Edit $cpdoConfig and set `$env:CPDO_GITHUB_PAT in your environment."
}
finally {
    Remove-Item -LiteralPath $cpdoTempDir -Recurse -Force -ErrorAction SilentlyContinue
}
