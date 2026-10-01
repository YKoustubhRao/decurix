<#
.SYNOPSIS
    Build the Decurix Windows Electron installer.
.DESCRIPTION
    Run in Windows PowerShell from any directory. Requires Go 1.26+, Node.js
    and npm. The resulting installer is under dist/electron/. Docker Desktop
    is needed when running the installed app, not while building it.
#>
param()

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$electron = Join-Path $repo 'desktop/electron'
$stage = Join-Path $electron 'build'

# npm.cmd uses cmd.exe for lifecycle scripts, and cmd.exe cannot use a WSL UNC
# path as its working directory. Build from a Windows drive checkout.
if ($repo.StartsWith('\\')) {
    throw 'Copy the repository to a Windows drive (for example C:\src\decurix) before building. npm cannot build from a WSL UNC path.'
}

$os = Get-CimInstance Win32_OperatingSystem
$freeMemoryMB = [int]($os.FreePhysicalMemory / 1024)
if ($freeMemoryMB -lt 3072) {
    throw "Only $freeMemoryMB MB of RAM is free. Close other apps until at least 3072 MB is free before building."
}

foreach ($tool in @('go', 'node', 'npm')) {
    if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) {
        throw "$tool is required to build the Windows app."
    }
}

Write-Host 'Checking Electron launcher...'
& node --test (Join-Path $electron 'main.test.js') (Join-Path $electron 'mobile-access.test.js')
if ($LASTEXITCODE -ne 0) { throw 'Electron launcher tests failed.' }

$version = 'dev'
if (Get-Command git -ErrorAction SilentlyContinue) {
    $description = git -C $repo describe --tags --always --dirty 2>$null
    if ($LASTEXITCODE -eq 0 -and $description) { $version = $description.Trim() }
}

New-Item -ItemType Directory -Force -Path $stage | Out-Null
Push-Location $repo
$oldGoos = $env:GOOS
$oldGoarch = $env:GOARCH
$oldCgo = $env:CGO_ENABLED
$oldGoFlags = $env:GOFLAGS
$oldGoMaxProcs = $env:GOMAXPROCS
$oldGoMemLimit = $env:GOMEMLIMIT
try {
    $env:CGO_ENABLED = '0'
    $env:GOFLAGS = '-p=1'
    $env:GOMAXPROCS = '2'
    $env:GOMEMLIMIT = '512MiB'
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    Write-Host 'Building Windows control plane...'
    & go build -trimpath -ldflags "-s -w -X main.version=$version" -o (Join-Path $stage 'decurix.exe') ./cmd/decurix
    if ($LASTEXITCODE -ne 0) { throw 'Windows Go build failed.' }

    $env:GOOS = 'android'
    $env:GOARCH = 'arm64'
    Write-Host 'Building Android phone helper...'
    & go build -trimpath -ldflags "-s -w -X main.version=$version" -o (Join-Path $stage 'decurix-android-arm64') ./cmd/decurix
    if ($LASTEXITCODE -ne 0) { throw 'Android Go build failed.' }

} finally {
    $env:GOOS = $oldGoos
    $env:GOARCH = $oldGoarch
    $env:CGO_ENABLED = $oldCgo
    $env:GOFLAGS = $oldGoFlags
    $env:GOMAXPROCS = $oldGoMaxProcs
    $env:GOMEMLIMIT = $oldGoMemLimit
    Pop-Location
}

Push-Location $electron
$oldNodeOptions = $env:NODE_OPTIONS
$oldNpmSockets = $env:npm_config_maxsockets
try {
    if (-not $env:NODE_OPTIONS) { $env:NODE_OPTIONS = '--max-old-space-size=1024' }
    $env:npm_config_maxsockets = '2'
    Write-Host 'Installing pinned Electron dependencies...'
    & npm ci
    if ($LASTEXITCODE -ne 0) { throw 'npm ci failed.' }
    Write-Host 'Building Windows installer...'
    & npm run dist:win
    if ($LASTEXITCODE -ne 0) { throw 'Electron installer build failed.' }
} finally {
    $env:NODE_OPTIONS = $oldNodeOptions
    $env:npm_config_maxsockets = $oldNpmSockets
    Pop-Location
}

Write-Host "Installer: $(Join-Path $repo 'dist/electron')"
