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

function Test-ImageExists {
    param([string]$Tag)
    docker image inspect $Tag *>$null
    return ($LASTEXITCODE -eq 0)
}

function Build-DesktopImage {
    param([string]$Tag, [string]$Dockerfile, [string]$Label)

    if ($OnlyMissing -and (Test-ImageExists $Tag)) {
        Write-Host "=== $Label desktop image already exists, skipping ($Tag) ==="
        Write-Host ""
        return
    }

    Write-Host "=== Building $Label desktop image ==="

    docker build -f $Dockerfile -t $Tag .

    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    Write-Host ""
}

Build-DesktopImage -Tag "flowera-desktop:basic" -Dockerfile "docker/desktop.basic.Dockerfile" -Label "Basic"
Build-DesktopImage -Tag "flowera-desktop:developer" -Dockerfile "docker/desktop.developer.Dockerfile" -Label "Developer"
Build-DesktopImage -Tag "flowera-desktop:ai" -Dockerfile "docker/desktop.ai.Dockerfile" -Label "AI Developer"

Write-Host "=== Desktop images built ==="
docker images "flowera-desktop"
Write-Host ""
