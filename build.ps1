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
#endregion

#region Main Script
Write-Host "========================================"
Write-Host " Building Flowera desktop images"
Write-Host "========================================"
Write-Host ""

Write-Host "=== Building Basic desktop image ==="
docker build -f DesktopDockerfile.basic -t flowera-desktop:basic .
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

Write-Host ""
Write-Host "=== Building Developer desktop image ==="
docker build -f DesktopDockerfile.developer -t flowera-desktop:developer .
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

Write-Host ""
Write-Host "=== Building AI Developer desktop image ==="
docker build -f DesktopDockerfile.ai -t flowera-desktop:ai .
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

Write-Host ""
Write-Host "=== Desktop images built ==="
docker images "flowera-desktop"
Write-Host ""

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
cloudflared tunnel --url http://127.0.0.1:8080
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
#endregion
