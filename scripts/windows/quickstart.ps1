<#
.SYNOPSIS
    One-shot: takes a fresh Windows checkout all the way to a running
    Flowera dashboard.
.DESCRIPTION
    Chains the two steps README.md documents as separate manual steps -
    scripts/windows/setup.ps1 (host dependencies, needs Administrator,
    skipped if Docker is already installed) and scripts/windows/build.ps1
    (build every image, start the control plane, wait for the dashboard,
    open a Cloudflare tunnel) - into a single command for a first-time
    setup.

    Already have Docker? This just runs build.ps1. Already have Flowera
    running and want to rebuild without touching your desktops? Use
    build.ps1 directly - see README.md's "Everything" row for exactly what
    it tears down and why.
#>

$ErrorActionPreference = "Stop"

# Every script runs from the repo root, two levels up from scripts/<group>/.
Set-Location (Split-Path -Parent (Split-Path -Parent $PSScriptRoot))

Write-Host "========================================"
Write-Host " Flowera quickstart"
Write-Host "========================================"
Write-Host ""

$dockerReady = $false
if (Get-Command docker -ErrorAction SilentlyContinue) {
    docker info *>$null
    $dockerReady = ($LASTEXITCODE -eq 0)
}

if ($dockerReady) {
    Write-Host "Docker is already installed and running - skipping host setup."
} else {
    Write-Host "Docker not found or not running - running host setup (needs Administrator)."
    Write-Host ""

    $isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)

    if (-not $isAdmin) {
        Write-Host "ERROR: this shell is not running as Administrator."
        Write-Host "Open PowerShell as Administrator and run:"
        Write-Host "  .\scripts\windows\setup.ps1"
        Write-Host "Then re-run:"
        Write-Host "  .\scripts\windows\quickstart.ps1"
        exit 1
    }

    & "$PSScriptRoot/setup.ps1"
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

    Write-Host ""
    Write-Host "Host setup finished. Docker Desktop may need a restart or a"
    Write-Host "sign-out/sign-in to finish installing. Once 'docker info'"
    Write-Host "works in a new shell, re-run:"
    Write-Host "  .\scripts\windows\quickstart.ps1"
    exit 0
}

Write-Host ""
& "$PSScriptRoot/build.ps1"
exit $LASTEXITCODE
