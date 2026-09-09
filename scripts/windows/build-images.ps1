<#
.SYNOPSIS
    Builds the three Flowera desktop images (basic / developer / ai).
.DESCRIPTION
    Called by scripts/windows/build.ps1. This is the repo-checkout path; a
    downloaded release executable does not need it, because it embeds the same
    build context and builds the images itself via `flowera --build-images`.
#>

# Find the repo root by walking up from this script until the desktop
# Dockerfiles come into view, so it runs correctly from any directory.
$root = $PSScriptRoot

while ($root -and -not (Test-Path (Join-Path $root "docker/desktop.basic.Dockerfile"))) {
    $root = Split-Path -Parent $root
}

if (-not $root) {
    Write-Host "ERROR: no docker/desktop.*.Dockerfile found above $PSScriptRoot."
    exit 1
}

Set-Location $root

Write-Host "========================================"
Write-Host " Building Flowera desktop images"
Write-Host "========================================"
Write-Host ""

Write-Host "=== Building Basic desktop image ==="
docker build -f docker/desktop.basic.Dockerfile -t flowera-desktop:basic .
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

Write-Host ""
Write-Host "=== Building Developer desktop image ==="
docker build -f docker/desktop.developer.Dockerfile -t flowera-desktop:developer .
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

Write-Host ""
Write-Host "=== Building AI Developer desktop image ==="
docker build -f docker/desktop.ai.Dockerfile -t flowera-desktop:ai .
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

Write-Host ""
Write-Host "=== Desktop images built ==="
docker images "flowera-desktop"
Write-Host ""
