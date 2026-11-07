<#
.SYNOPSIS
    Builds the two Decurix images: the desktop (decurix-desktop:basic) and
    the shared tools runtime (decurix-tools:latest); developer and ai are
    only tags of basic.
.DESCRIPTION
    Called by scripts/windows/build.ps1 and scripts/windows/rebuild.ps1. This
    is the repo-checkout path; a downloaded release executable does not need
    it, because it embeds the same build context and builds the images
    itself via `decurix --build-images`.
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
Write-Host " Building Decurix desktop images"
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

Build-DesktopImage -Tag "decurix-desktop:basic" -Dockerfile "docker/desktop.basic.Dockerfile" -Label "Unified Desktop Base"
Build-DesktopImage -Tag "decurix-tools:latest" -Dockerfile "docker/tools.Dockerfile" -Label "Shared Tools Runtime"

# Each step's exit code is checked, as the .sh twins' `set -e` does: an image
# that was not tagged, or a tools volume that was not filled, must not be
# reported as ready.
function Invoke-Step([string]$What, [scriptblock]$Step) {
    & $Step
    if ($LASTEXITCODE -ne 0) {
        Write-Host "ERROR: could not $What (exit $LASTEXITCODE)."
        exit $LASTEXITCODE
    }
}

Write-Host "=== Aliasing desktop profiles (developer / ai) to unified base ==="
Invoke-Step 'tag decurix-desktop:developer' { docker tag decurix-desktop:basic decurix-desktop:developer }
Invoke-Step 'tag decurix-desktop:ai' { docker tag decurix-desktop:basic decurix-desktop:ai }

Write-Host ""
Write-Host "=== Populating shared tools volume (decurix-tools) ==="
Invoke-Step 'create the decurix-tools volume' { docker volume create decurix-tools *>$null }
Invoke-Step 'populate the decurix-tools volume' { docker run --rm -v decurix-tools:/target decurix-tools:latest }
Invoke-Step 'create the decurix-cache volume' { docker volume create decurix-cache *>$null }

Write-Host ""
Write-Host "=== Pruning dangling build layers to reclaim disk space ==="
docker image prune -f *>$null

Write-Host ""
Write-Host "=== Desktop and tools images ready ==="
docker images --filter "reference=decurix-*"
Write-Host ""

