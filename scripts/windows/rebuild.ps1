<#
.SYNOPSIS
    Rebuilds Decurix from a clean slate.
.DESCRIPTION
    The Windows twin of scripts/linux/rebuild.sh: no leftover desktop_*
    containers or volumes, and an EMPTY app database (decurix-data: task
    history, chat transcripts, query notes, worker memory). Images are still
    rebuilt (so a change to agent/ or a Dockerfile is picked up), but with
    Docker's normal build cache, not --no-cache - a rebuild should be
    thorough about clearing out data, not slow. This is the "reinstall"
    script - for "just start what I already have", use
    scripts/windows/build.ps1 instead.

    What is deliberately NOT wiped:
      - .env (your configuration - API keys, OLLAMA_URL/MODEL, etc.)
      - the decurix_ollama volume (downloaded models - re-pulling a chat
        model is a multi-GB download nobody wants on every rebuild)

    Needs Docker Desktop running. Does not need Administrator.
.PARAMETER Yes
    Skip the confirmation prompt (for scripted/CI use).
.PARAMETER Cli
    Skip the tunnel and open the terminal CLI instead (docker compose exec
    desktop-manager ./decurix --cli), a text alternative to the dashboard.
#>

param(
    [switch]$Yes,
    [switch]$Cli
)

#region Functions
function Test-CommandAvailable {
    param([string]$CommandName)
    [bool](Get-Command $CommandName -ErrorAction SilentlyContinue -CommandType Application)
}

# Exit-Build is how every deliberate exit leaves the script. It marks the
# exit as intentional so the `finally` below leaves the stack alone once a
# rebuild has actually finished starting Decurix back up - only Ctrl+C
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

# However far the rebuild got, an interrupted one should not leave a stack
# half old, half fresh - tear down whatever is currently there rather than
# leave it running detached. Runs from a `finally` because PowerShell has no
# INT-only trap.
function Invoke-Cleanup {
    Write-Host ""
    Stop-LanTunnel
    Write-Host "=== Interrupted: stopping Decurix and its containers ==="

    docker compose down

    $containers = docker ps -a --filter "name=desktop_" --format "{{.ID}}"
    if ($containers) {
        docker rm -f $containers
    }

    docker rm -f decurix-ollama 2>$null | Out-Null
}
#endregion

#region Main Script
# Every script runs from the repo root, two levels up from scripts/<group>/.
Set-Location (Split-Path -Parent (Split-Path -Parent $PSScriptRoot))

. "$PSScriptRoot/lib/Ollama-Setup.ps1"
. "$PSScriptRoot/lib/Lan-Tunnel.ps1"
. "$PSScriptRoot/lib/Keep-Awake.ps1"

$script:IntentionalExit = $false

Write-Host "========================================"
Write-Host " Decurix rebuild (clean slate)"
Write-Host "========================================"
Write-Host ""
Write-Host "This will permanently delete:"
Write-Host "  - every desktop_* container and volume"
Write-Host "  - the decurix-data volume: ALL task history, chats, query notes and"
Write-Host "    worker memory - this cannot be undone"
Write-Host ""
Write-Host "Kept as-is: .env, and the decurix_ollama volume (your pulled models)."
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
    Write-Host "=== Stopping Decurix ==="
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
    Write-Host "=== Wiping the app database (decurix-data) ==="
    docker volume rm decurix-data 2>$null | Out-Null

    Write-Host ""
    Write-Host "=== Removing the bundled Ollama container (keeping its model volume) ==="
    docker rm -f decurix-ollama 2>$null | Out-Null

    & "$PSScriptRoot/build-images.ps1"
    if ($LASTEXITCODE -ne 0) { Exit-Build $LASTEXITCODE }

    Write-Host ""
    Write-Host "=== Ensuring the desktops can reach Ollama ==="

    Invoke-EnsureOllamaReady

    Write-Host ""
    Write-Host "=== Starting Decurix ==="
    docker compose up -d --build
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
    Write-Host " Decurix is running (clean slate)"
    Write-Host "========================================"
    Write-Host ""
    Write-Host "Dashboard: http://localhost:8080"
    Write-Host "Idle desktops stop automatically after 5 minutes (set DECURIX_IDLE_STOP_MINUTES=0 in .env to disable, or to another number of minutes)."

    # DECURIX_LAN_PORT in .env changes which port the access-code-gated
    # listener binds.
    $lanPort = Read-EnvVar -Name "DECURIX_LAN_PORT"
    if (-not $lanPort) { $lanPort = "8090" }

    # The machine's own address on whatever network it's on, so both the
    # admin (unauthenticated) and LAN (access-code-gated) URLs below are
    # something another device can actually use - not "localhost", which
    # only means anything on this machine. Opening a UDP "connection" sends
    # no packets; it only asks Windows which local address it would use to
    # reach the internet, the same trick build.sh's "ip route get" does.
    $lanIP = $null
    try {
        $socket = New-Object System.Net.Sockets.Socket(
            [System.Net.Sockets.AddressFamily]::InterNetwork,
            [System.Net.Sockets.SocketType]::Dgram,
            [System.Net.Sockets.ProtocolType]::Udp)
        $socket.Connect("1.1.1.1", 65530)
        $lanIP = $socket.LocalEndPoint.Address.ToString()
        $socket.Close()
    } catch {
        # No route to the internet, or sockets unavailable - $lanIP stays unset.
    }

    if ($lanIP) {
        $lanUrl = "http://${lanIP}:$lanPort"

        # Reported once here, separately from the tunnel URL below (which is
        # reported every time cloudflared prints one) - see
        # httpapi.tunnelHandler, which never lets one blank out the other.
        try {
            Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:8080/api/tunnel" `
                -ContentType "application/json" `
                -Body (@{ lan_url = $lanUrl } | ConvertTo-Json -Compress) `
                -TimeoutSec 5 | Out-Null
        } catch {
            # Best effort - the dashboard's Mobile menu is the fallback.
        }

        Write-Host ""
        Write-Host "LAN access URL (needs the code below, works without the tunnel): $lanUrl"
    }

    # Printed here as a convenience - it also always lives in the
    # dashboard's Mobile menu (both URLs above alongside it), which is the
    # one place it stays current after a rotate.
    $lanCode = $null
    try {
        $lan = Invoke-RestMethod -Uri "http://127.0.0.1:8080/api/lan-access/status" -TimeoutSec 5
        if ($lan.code) {
            $lanCode = $lan.code
            Write-Host ""
            Write-Host "Access code: $lanCode"
            Write-Host "(Needed once by anyone using the LAN URL above. Also in the dashboard's Mobile menu, which can generate a new one.)"

            # A one-click link for whoever is sitting at this terminal right
            # now - they already have the code above, so this authenticates
            # on click instead of asking them to type it into the gate's form.
            if ($lanUrl) {
                Write-Host ""
                Write-Host "Open it yourself without typing the code: $lanUrl/__decurix_access/authenticate?code=$lanCode"
            }
        }
    } catch {
        # Best effort - the dashboard's Mobile menu always has it too.
    }

    # The rebuild itself is done at this point (fresh images, fresh
    # desktops, empty database, dashboard responding). Exit-Build marks this
    # as intentional so finally{}'s Invoke-Cleanup - which tears the whole
    # stack back down, for an interrupted *rebuild* - is skipped: leaving the
    # CLI should just leave Decurix running, not undo the rebuild that just
    # finished.
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
        Write-Host "ERROR: cloudflared is not installed."
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
