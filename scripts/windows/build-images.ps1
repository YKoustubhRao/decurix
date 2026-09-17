<#
.SYNOPSIS
    Builds the three Flowera desktop images (basic / developer / ai).
.DESCRIPTION
    Called by scripts/windows/build.ps1 and scripts/windows/rebuild.ps1. This
    is the repo-checkout path; a downloaded release executable does not need
    it, because it embeds the same build context and builds the images
    itself via `flowera --build-images`.
.PARAMETER OnlyMissing
    Skip an image whose tag already exists (build.ps1's "just open the app" -
    don't pay for a rebuild nobody asked for).
#>

param(
    [switch]$OnlyMissing
)

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

$env:DOCKER_BUILDKIT = "1"

function Test-ImageExists {
    param([string]$Tag)
    docker image inspect $Tag *>$null
    return ($LASTEXITCODE -eq 0)
}

function Build-DesktopImage {
    param([string]$Tag, [string]$Dockerfile, [string]$Label)

    if ($OnlyMissing -and (Test-ImageExists $Tag)) {
        Write-Host "=== $Label image already exists, skipping ($Tag) ==="
        Write-Host ""
        return
    }

    Write-Host "=== Building $Label image ==="

    docker build -f $Dockerfile -t $Tag .

    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    Write-Host ""
}

Build-DesktopImage -Tag "flowera-desktop:basic" -Dockerfile "docker/desktop.basic.Dockerfile" -Label "Unified Desktop Base"
Build-DesktopImage -Tag "flowera-tools:latest" -Dockerfile "docker/tools.Dockerfile" -Label "Shared Tools Runtime"

Write-Host "=== Aliasing desktop profiles (developer / ai) to unified base ==="
docker tag flowera-desktop:basic flowera-desktop:developer
docker tag flowera-desktop:basic flowera-desktop:ai

Write-Host ""
Write-Host "=== Populating shared tools volume (flowera-tools) ==="
docker volume create flowera-tools *>$null
docker run --rm -v flowera-tools:/target flowera-tools:latest
docker volume create flowera-cache *>$null

Write-Host ""
Write-Host "=== Pruning dangling build layers to reclaim disk space ==="
docker image prune -f *>$null

Write-Host ""
Write-Host "=== Desktop and tools images ready ==="
docker images --filter "reference=flowera-*"
Write-Host ""

