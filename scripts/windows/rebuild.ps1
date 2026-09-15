<#
.SYNOPSIS
    Rebuilds Flowera from a clean slate.
.DESCRIPTION
    The Windows twin of scripts/linux/rebuild.sh: no leftover desktop_*
    containers or volumes, and an EMPTY app database (flowera-data: task
    history, chat transcripts, query notes, worker memory). Images are still
    rebuilt (so a change to agent/ or a Dockerfile is picked up), but with
    Docker's normal build cache, not --no-cache - a rebuild should be
    thorough about clearing out data, not slow. This is the "reinstall"
    script - for "just start what I already have", use
    scripts/windows/build.ps1 instead.

    What is deliberately NOT wiped:
      - .env (your configuration - API keys, OLLAMA_URL/MODEL, etc.)
      - the flowera_ollama volume (downloaded models - re-pulling a chat
        model is a multi-GB download nobody wants on every rebuild)

    Needs Docker Desktop running. Does not need Administrator.
.PARAMETER Yes
    Skip the confirmation prompt (for scripted/CI use).
#>

param(
    [switch]$Yes
)

#region Functions
function Test-CommandAvailable {
    param([string]$CommandName)
    [bool](Get-Command $CommandName -ErrorAction SilentlyContinue -CommandType Application)
}

# Exit-Build is how every deliberate exit leaves the script. It marks the
# exit as intentional so the `finally` below leaves the stack alone once a
# rebuild has actually finished starting Flowera back up - only Ctrl+C
# during the confirm/rebuild/tunnel phases should trigger the teardown.
function Exit-Build {
    param([int]$Code)
    $script:IntentionalExit = $true
    exit $Code
}

function Wait-ForDashboard {
    param([int]$MaxAttempts = 60, [int]$DelaySeconds = 2)

    for ($attempt = 1; $attempt -le $MaxAttempts; $attempt++) {
        try {
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

# However far the rebuild got, an interrupted one should not leave a stack
# half old, half fresh - tear down whatever is currently there rather than
# leave it running detached. Runs from a `finally` because PowerShell has no
# INT-only trap.
function Invoke-Cleanup {
    Write-Host ""
    Write-Host "=== Interrupted: stopping Flowera and its containers ==="

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

. "$PSScriptRoot/lib/Ollama-Setup.ps1"

$script:IntentionalExit = $false

Write-Host "========================================"
Write-Host " Flowera rebuild (clean slate)"
Write-Host "========================================"
Write-Host ""
Write-Host "This will permanently delete:"
Write-Host "  - every desktop_* container and volume"
Write-Host "  - the flowera-data volume: ALL task history, chats, query notes and"
Write-Host "    worker memory - this cannot be undone"
Write-Host ""
Write-Host "Kept as-is: .env, and the flowera_ollama volume (your pulled models)."
Write-Host ""

if (-not $Yes) {
    if ([Console]::IsInputRedirected) {
        Write-Host "ERROR: not running interactively. Re-run with -Yes to confirm."
        exit 1
    }

    $confirm = Read-Host "Type 'yes' to continue"
    if ($confirm -ne "yes") {
        Write-Host "Aborted. Nothing was changed."
        exit 1
    }
}

try {
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

    # A fresh clone has no .env at all, and "docker compose up --build"
    # fails without one. Start from the example so OLLAMA_MODEL and friends
    # are there. An existing .env is left exactly as it is - a rebuild is
    # not a reset of your configuration.
    if (-not (Test-Path $envFile)) {
        if (Test-Path ".env.example") {
            Write-Host "No .env yet - creating it from .env.example."
            Copy-Item ".env.example" $envFile
        } else {
            New-Item -ItemType File -Path $envFile | Out-Null
        }
    }

    Write-Host ""
    Write-Host "=== Stopping Flowera ==="
    docker compose down
    if ($LASTEXITCODE -ne 0) { Exit-Build $LASTEXITCODE }

    Write-Host ""
    Write-Host "=== Removing old desktop containers ==="
    $containers = docker ps -a --filter "name=desktop_" --format "{{.ID}}"
    if ($containers) {
        docker rm -f $containers
        if ($LASTEXITCODE -ne 0) { Exit-Build $LASTEXITCODE }
    }

    Write-Host ""
    Write-Host "=== Removing old desktop volumes ==="
    $volumes = docker volume ls --filter "name=desktop_" --format "{{.Name}}"
    if ($volumes) {
        docker volume rm $volumes
        if ($LASTEXITCODE -ne 0) { Exit-Build $LASTEXITCODE }
    }

    Write-Host ""
    Write-Host "=== Wiping the app database (flowera-data) ==="
    docker volume rm flowera-data 2>$null | Out-Null

    Write-Host ""
    Write-Host "=== Removing the bundled Ollama container (keeping its model volume) ==="
    docker rm -f flowera-ollama 2>$null | Out-Null

    & "$PSScriptRoot/build-images.ps1"
    if ($LASTEXITCODE -ne 0) { Exit-Build $LASTEXITCODE }

    Write-Host ""
    Write-Host "=== Ensuring the desktops can reach Ollama ==="

    Invoke-EnsureOllamaReady

    Write-Host ""
    Write-Host "=== Starting Flowera ==="
    docker compose up -d --build
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
    Write-Host " Flowera is running (clean slate)"
    Write-Host "========================================"
    Write-Host ""
    Write-Host "Dashboard: http://localhost:8080"

    Write-Host ""
    Write-Host "=== Starting Cloudflare Tunnel ==="
    if (-not (Test-CommandAvailable "cloudflared")) {
        Write-Host "ERROR: cloudflared is not installed."
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
