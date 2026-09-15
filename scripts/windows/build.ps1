<#
.SYNOPSIS
    Opens Flowera: builds whichever desktop images don't exist yet, makes
    sure a desktop can reach Ollama, starts the control plane if it isn't
    already running, and opens a Cloudflare tunnel to it.
.DESCRIPTION
    The Windows twin of scripts/linux/build.sh. Existing desktops, their
    volumes and the app database are left exactly as they are - this is
    meant to feel like reopening an app, not reinstalling it.

    Want a clean slate instead (fresh images, fresh desktops, empty
    database)? Use scripts/windows/rebuild.ps1.

    Needs Docker Desktop running. Does not need Administrator.

    Ctrl+C while it runs (including while the tunnel is up) just stops
    watching the tunnel - it does not tear Flowera down.
.PARAMETER Cli
    Skip the tunnel and open the terminal CLI instead (docker compose exec
    desktop-manager ./flowera --cli), a text alternative to the dashboard.
#>

param(
    [switch]$Cli
)

#region Functions
function Test-CommandAvailable {
    param([string]$CommandName)
    [bool](Get-Command $CommandName -ErrorAction SilentlyContinue -CommandType Application)
}

# Exit-Build is how every deliberate exit leaves the script. It marks the
# exit as intentional so the `finally` below skips the "tunnel stopped"
# notice - that notice is for Ctrl+C, not for an error this script already
# explained itself.
function Exit-Build {
    param([int]$Code)
    $script:IntentionalExit = $true
    exit $Code
}

function Wait-ForDashboard {
    param([int]$MaxAttempts = 60, [int]$DelaySeconds = 2)

    for ($attempt = 1; $attempt -le $MaxAttempts; $attempt++) {
        try {
            # -UseBasicParsing: without it Windows PowerShell 5.1 hands the
            # response to the Internet Explorer engine, which fails outright
            # on a machine where IE was never set up.
            $response = Invoke-WebRequest -Uri "http://127.0.0.1:8080/" `
                -UseBasicParsing -TimeoutSec 5 -ErrorAction Stop
            if ($response.StatusCode -eq 200) {
                Write-Host "Flowera dashboard is running."
                return $true
            }
        } catch {
            # Not up yet.
        }

        Write-Host "Waiting for Flowera... ($attempt/$MaxAttempts)"
        Start-Sleep -Seconds $DelaySeconds
    }

    return $false
}

# Ctrl+C lands here while the script is blocked on the tunnel command, which
# runs forever until interrupted. Unlike rebuild.ps1, this script's whole
# point is to leave Flowera running, so interrupting it does not tear
# anything down - it only stops watching the tunnel. Runs from a `finally`
# because PowerShell has no INT-only trap; it also covers the tunnel exiting
# on its own.
function Invoke-Cleanup {
    Write-Host ""
    Write-Host "=== Tunnel stopped. Flowera is still running: http://127.0.0.1:8080 ==="
    Write-Host "(Run this script again for a new tunnel, or 'docker compose down' to stop Flowera.)"
}
#endregion

#region Main Script
# Every script runs from the repo root, two levels up from scripts/<group>/.
Set-Location (Split-Path -Parent (Split-Path -Parent $PSScriptRoot))

. "$PSScriptRoot/lib/Ollama-Setup.ps1"

$script:IntentionalExit = $false

try {
    Write-Host "========================================"
    Write-Host " Opening Flowera"
    Write-Host "========================================"
    Write-Host ""

    # The single most common failure on Windows, and docker's own message
    # for it (a missing named pipe) does not say "start Docker Desktop".
    if (-not (Test-CommandAvailable "docker")) {
        Write-Host "ERROR: docker is not installed. Run .\scripts\windows\setup.ps1"
        Write-Host "as Administrator, or install Docker Desktop yourself."
        Exit-Build 1
    }

    docker info *>$null
    if ($LASTEXITCODE -ne 0) {
        Write-Host "ERROR: Docker is installed but not running."
        Write-Host "Start Docker Desktop, wait until it says it is running, and re-run."
        Exit-Build 1
    }

    $envFile = Join-Path (Get-Location).Path ".env"

    # A fresh clone has no .env at all, and "docker compose up" fails
    # without one. Start from the example so OLLAMA_MODEL and friends are
    # there.
    if (-not (Test-Path $envFile)) {
        if (Test-Path ".env.example") {
            Write-Host "No .env yet - creating it from .env.example."
            Copy-Item ".env.example" $envFile
        } else {
            New-Item -ItemType File -Path $envFile | Out-Null
        }
    }

    # Only build an image that doesn't exist yet - that is the entire
    # difference between "open the app" and "rebuild the app". A missing
    # image (first run, or a profile added since) still gets built.
    & "$PSScriptRoot/build-images.ps1" -OnlyMissing
    if ($LASTEXITCODE -ne 0) { Exit-Build $LASTEXITCODE }

    Write-Host ""
    Write-Host "=== Ensuring the desktops can reach Ollama ==="

    Invoke-EnsureOllamaReady

    Write-Host ""
    Write-Host "=== Starting Flowera ==="
    # No --build: an already-built control plane image starts as-is.
    # Existing desktop_* containers and volumes, and the flowera-data
    # volume (task history, chats, worker memory), are untouched.
    docker compose up -d
    if ($LASTEXITCODE -ne 0) { Exit-Build $LASTEXITCODE }

    Write-Host ""
    Write-Host "=== Waiting for dashboard ==="

    if (-not (Wait-ForDashboard -MaxAttempts 60 -DelaySeconds 2)) {
        Write-Host "ERROR: Flowera dashboard is not responding on port 8080."
        docker compose logs --tail=100
        Exit-Build 1
    }

    Write-Host ""
    Write-Host "========================================"
    Write-Host " Flowera is running"
    Write-Host "========================================"
    Write-Host ""
    Write-Host "Dashboard: http://localhost:8080"

    # -Cli is a terminal-native alternative to the tunnel below - it never
    # opens a tunnel URL, so Invoke-Cleanup's "tunnel stopped" message would
    # not make sense here. Exit-Build marks this as intentional so that
    # finally{} skips it, same as every other deliberate exit.
    if ($Cli) {
        Write-Host ""
        Write-Host "=== Opening the Flowera CLI ==="
        Write-Host "(Flowera keeps running after you exit; 'exit' or Ctrl+D to leave.)"
        Write-Host ""

        docker compose exec desktop-manager ./flowera --cli
        Exit-Build $LASTEXITCODE
    }

    Write-Host ""
    Write-Host "=== Starting Cloudflare Tunnel ==="
    if (-not (Test-CommandAvailable "cloudflared")) {
        Write-Host "ERROR: cloudflared is not installed. Flowera is still running"
        Write-Host "locally; install cloudflared (.\scripts\windows\setup.ps1) for"
        Write-Host "access from other devices."
        Exit-Build 1
    }
    Write-Host ""

    # cloudflared logs to stderr, hence 2>&1. Echo every line while watching
    # for the trycloudflare.com URL, and report it to the control plane for
    # display in the dashboard's Mobile dropdown. "$_" rather than $_: in
    # Windows PowerShell 5.1 a redirected stderr line arrives as an
    # ErrorRecord, and only its string form is the line itself.
    cloudflared tunnel --protocol http2 --url http://127.0.0.1:8080 2>&1 | ForEach-Object {
        $line = "$_"
        Write-Host $line

        if ($line -match 'https://[a-zA-Z0-9.-]+\.trycloudflare\.com') {
            try {
                Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:8080/api/tunnel" `
                    -ContentType "application/json" `
                    -Body (@{ url = $Matches[0] } | ConvertTo-Json -Compress) `
                    -TimeoutSec 5 | Out-Null
            } catch {
                # Best effort: the tunnel works whether or not the dashboard
                # shows it.
            }
        }
    }
}
finally {
    if (-not $script:IntentionalExit) {
        Invoke-Cleanup
    }
}
#endregion
