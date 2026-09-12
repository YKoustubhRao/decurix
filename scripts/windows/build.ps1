<#
.SYNOPSIS
    Builds Flowera desktop images, stops/removes old containers/volumes, starts Flowera, and sets up Cloudflare Tunnel.
.DESCRIPTION
    This script automates the process of building Flowera desktop images (basic, developer, AI),
    cleaning up old Docker resources, starting Flowera, and establishing a Cloudflare Tunnel.
#>

# Requires -RunAsAdministrator for Docker operations

#region Functions
function Test-CommandAvailable {
    param([string]$CommandName)
    Get-Command $CommandName -ErrorAction SilentlyContinue -CommandType Application | Out-Null
}

function Wait-ForDashboard {
    param([int]$MaxAttempts = 60, [int]$DelaySeconds = 2)
    $url = "http://127.0.0.1:8080/"
    $attempt = 0

    while ($attempt -lt $MaxAttempts) {
        try {
            $response = Invoke-WebRequest -Uri $url -Method Head -ErrorAction Stop
            if ($response.StatusCode -eq 200) {
                Write-Host "Flowera dashboard is running."
                return $true
            }
        } catch {
            $attempt++
            Write-Host "Waiting for Flowera... ($attempt/$MaxAttempts)"
            Start-Sleep -Seconds $DelaySeconds
        }
    }
    return $false
}

# Ctrl+C lands here while the script is blocked on the tunnel command, which
# runs forever until interrupted. Without this, "docker compose up -d" and
# the desktop containers it depends on would keep running detached with no
# tunnel pointed at them - a silent, orphaned Flowera. Tear the whole stack
# down instead. Runs from a `finally` because PowerShell has no INT-only
# trap; it also covers the tunnel exiting on its own.
function Invoke-Cleanup {
    Write-Host ""
    Write-Host "=== Stopping Flowera and its containers ==="

    docker compose down

    $containers = docker ps -a --filter "name=desktop_" --format "{{.ID}}"
    if ($containers) {
        docker rm -f $containers
    }

    docker rm -f flowera-ollama 2>$null | Out-Null
}
#endregion

#region Main Script
# Every script runs from the repo root, two levels up from scripts/<group>/.
Set-Location (Split-Path -Parent (Split-Path -Parent $PSScriptRoot))

try {
    & "$PSScriptRoot/build-images.ps1"
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

    Write-Host "=== Stopping Flowera ==="
    docker compose down
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

    Write-Host ""
    Write-Host "=== Removing old desktop containers ==="
    $containers = docker ps -a --filter "name=desktop_" --format "{{.ID}}"
    if ($containers) {
        docker rm -f $containers
        if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    }

    Write-Host ""
    Write-Host "=== Removing old desktop volumes ==="
    $volumes = docker volume ls --filter "name=desktop_" --format "{{.Name}}"
    if ($volumes) {
        docker volume rm $volumes
        if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    }

    Write-Host ""
    Write-Host "=== Starting Flowera ==="
    docker compose up --build -d
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

    Write-Host ""
    Write-Host "=== Waiting for dashboard ==="
    Write-Host "=== Waiting for dashboard ==="

    if (-not (Wait-ForDashboard -MaxAttempts 60 -DelaySeconds 2)) {
        Write-Host "ERROR: Flowera dashboard is not responding on port 8080."
        docker compose logs --tail=100
        exit 1
    }

    Write-Host ""
    Write-Host "========================================"
    Write-Host " Flowera is running"
    Write-Host "========================================"
    Write-Host ""

    Write-Host "=== Starting Cloudflare Tunnel ==="
    if (-not (Test-CommandAvailable "cloudflared")) {
        Write-Host "ERROR: cloudflared is not installed."
        exit 1
    }
    Write-Host ""

    # Start Cloudflare tunnel
    cloudflared tunnel --protocol http2 --url http://127.0.0.1:8080
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
}
finally {
    Invoke-Cleanup
}
#endregion
