<#
.SYNOPSIS
    Opens Decurix: builds whichever desktop images don't exist yet, makes
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
    watching the tunnel - it does not tear Decurix down.
.PARAMETER Cli
    Skip the tunnel and open the terminal CLI instead (docker compose exec
    desktop-manager ./decurix --cli), a text alternative to the dashboard.
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
                Write-Host "Decurix dashboard is running."
                return $true
            }
        } catch {
            # Not up yet.
        }

        Write-Host "Waiting for Decurix... ($attempt/$MaxAttempts)"
        Start-Sleep -Seconds $DelaySeconds
    }

    return $false
}

# Ctrl+C lands here while the script is blocked on the tunnel command, which
# runs forever until interrupted. Unlike rebuild.ps1, this script's whole
# point is to leave Decurix running, so interrupting it does not tear
# anything down - it only stops watching the tunnel. Runs from a `finally`
# because PowerShell has no INT-only trap; it also covers the tunnel exiting
# on its own.
function Invoke-Cleanup {
    Write-Host ""
    Stop-LanTunnel
    Write-Host "=== Tunnel stopped. Decurix is still running: http://127.0.0.1:8080 ==="
    Write-Host "(Run this script again for a new tunnel, or 'docker compose down' to stop Decurix.)"
}
#endregion

#region Main Script
# Every script runs from the repo root, two levels up from scripts/<group>/.
Set-Location (Split-Path -Parent (Split-Path -Parent $PSScriptRoot))

. "$PSScriptRoot/lib/Ollama-Setup.ps1"
. "$PSScriptRoot/lib/Lan-Tunnel.ps1"
. "$PSScriptRoot/lib/Keep-Awake.ps1"

$script:IntentionalExit = $false

try {
    Write-Host "========================================"
    Write-Host " Opening Decurix"
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
    Write-Host "=== Starting Decurix ==="
    # No --build: an already-built control plane image starts as-is.
    # Existing desktop_* containers and volumes, and the decurix-data
    # volume (task history, chats, worker memory), are untouched.
    docker compose $script:ComposeFiles up -d
    if ($LASTEXITCODE -ne 0) { Exit-Build $LASTEXITCODE }

    Write-Host ""
    Write-Host "=== Waiting for dashboard ==="

    if (-not (Wait-ForDashboard -MaxAttempts 60 -DelaySeconds 2)) {
        Write-Host "ERROR: Decurix dashboard is not responding on port 8080."
        docker compose logs --tail=100
        Exit-Build 1
    }

    Write-Host ""
    Write-Host "========================================"
    Write-Host " Decurix is running"
    Write-Host "========================================"
    Write-Host ""
    Write-Host "Dashboard: http://localhost:8080"
    Write-Host "Idle desktops stop automatically after 5 minutes (set DECURIX_IDLE_STOP_MINUTES=0 in .env to disable, or to another number of minutes)."

    # DECURIX_LAN_PORT in .env changes which port the access-code-gated
    # listener binds.
    $lanPort = Read-EnvVar -Name "DECURIX_LAN_PORT"
    if (-not $lanPort) { $lanPort = "8090" }

    Show-LanAccess -LanPort $lanPort

    # -Cli is a terminal-native alternative to the tunnel below - it never
    # opens a tunnel URL, so Invoke-Cleanup's "tunnel stopped" message would
    # not make sense here. Exit-Build marks this as intentional so that
    # finally{} skips it, same as every other deliberate exit.
    # Nothing runs while this machine sleeps - no email trigger, no
    # schedule - and the control plane, in its container, cannot stop
    # Windows sleeping. Held only while the dashboard has something that
    # needs it (lib/Keep-Awake.ps1); released in finally{}, however this
    # script ends.
    Write-Host ""
    Start-KeepAwake

    if ($Cli) {
        Write-Host ""
        Write-Host "=== Opening the Decurix CLI ==="
        Write-Host "(Decurix keeps running after you exit; 'exit' or Ctrl+D to leave.)"
        Write-Host ""

        docker compose exec desktop-manager ./decurix --cli
        Exit-Build $LASTEXITCODE
    }

    Write-Host ""
    Write-Host "=== Starting Cloudflare Tunnel ==="
    if (-not (Test-CommandAvailable "cloudflared")) {
        Write-Host "ERROR: cloudflared is not installed. Decurix is still running"
        Write-Host "locally; install cloudflared (.\scripts\windows\setup.ps1) for"
        Write-Host "access from other devices."
        Exit-Build 1
    }

    # A second tunnel, to the access-code-gated listener - so the
    # code-protected URL works from anywhere, not just $lanUrl above on this
    # machine's own network. Backgrounded (see lib/Lan-Tunnel.ps1), so it
    # the loop below only keeps this script in the foreground.
    Start-LanTunnel -LanPort $lanPort

    Write-Host ""

    # The primary listener (:8080) is deliberately never tunnelled: it has no
    # login and can create desktops and run commands on this machine. Only
    # the access-code-gated listener is reachable from off this machine. Its
    # tunnel runs in the background (lib/Lan-Tunnel.ps1); this just keeps the
    # script - and so its Ctrl+C handling - in the foreground.
    while ($true) {
        Start-Sleep -Seconds 5
    }
}
finally {
    Stop-KeepAwake

    if (-not $script:IntentionalExit) {
        Invoke-Cleanup
    }
}
#endregion
