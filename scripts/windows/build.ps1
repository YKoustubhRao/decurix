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
    Stop-LanTunnel
    Write-Host "=== Tunnel stopped. Flowera is still running: http://127.0.0.1:8080 ==="
    Write-Host "(Run this script again for a new tunnel, or 'docker compose down' to stop Flowera.)"
}
#endregion

#region Main Script
# Every script runs from the repo root, two levels up from scripts/<group>/.
Set-Location (Split-Path -Parent (Split-Path -Parent $PSScriptRoot))

. "$PSScriptRoot/lib/Ollama-Setup.ps1"
. "$PSScriptRoot/lib/Lan-Tunnel.ps1"

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
    Write-Host "Idle desktops stop automatically after 5 minutes (set FLOWERA_IDLE_STOP_MINUTES=0 in .env to disable, or to another number of minutes)."

    # FLOWERA_LAN_PORT in .env changes which port the access-code-gated
    # listener binds.
    $lanPort = Read-EnvVar -Name "FLOWERA_LAN_PORT"
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
        $adminUrl = "http://${lanIP}:8080"
        $lanUrl = "http://${lanIP}:$lanPort"

        # Reported once here, separately from the tunnel URL below (which is
        # reported every time cloudflared prints one) - see
        # httpapi.tunnelHandler, which never lets one blank out the other.
        try {
            Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:8080/api/tunnel" `
                -ContentType "application/json" `
                -Body (@{ admin_url = $adminUrl; lan_url = $lanUrl } | ConvertTo-Json -Compress) `
                -TimeoutSec 5 | Out-Null
        } catch {
            # Best effort - the dashboard's Mobile menu is the fallback.
        }

        Write-Host ""
        Write-Host "Admin URL (full control, no code - keep this to yourself): $adminUrl"
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
                Write-Host "Open it yourself without typing the code: $lanUrl/__flowera_access/authenticate?code=$lanCode"
            }
        }
    } catch {
        # Best effort - the dashboard's Mobile menu always has it too.
    }

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

    # A second tunnel, to the access-code-gated listener - so the
    # code-protected URL works from anywhere, not just $lanUrl above on this
    # machine's own network. Backgrounded (see lib/Lan-Tunnel.ps1), so it
    # doesn't block the primary tunnel's own foreground loop just below.
    Start-LanTunnel -LanPort $lanPort

    Write-Host ""

    # Points at the primary listener (:8080), not the access-code-gated one -
    # so this URL is full, unauthenticated control of the machine, reachable
    # by anyone who has the link, the same as sitting at the machine itself.
    # The gated URL printed above ($lanUrl, over $lanPort) and its own tunnel
    # link just above are still there for anyone who wants the code-gated
    # option instead.

    # cloudflared logs to stderr, hence 2>&1. Echo every line while watching
    # for the trycloudflare.com URL, and report it to the control plane for
    # display in the dashboard's Mobile dropdown. "$_" rather than $_: in
    # Windows PowerShell 5.1 a redirected stderr line arrives as an
    # ErrorRecord, and only its string form is the line itself.
    cloudflared tunnel --protocol http2 --url "http://127.0.0.1:8080" 2>&1 | ForEach-Object {
        $line = "$_"
        Write-Host $line

        if ($line -match 'https://[a-zA-Z0-9.-]+\.trycloudflare\.com') {
            $tunnelUrl = $Matches[0]
            try {
                Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:8080/api/tunnel" `
                    -ContentType "application/json" `
                    -Body (@{ url = $tunnelUrl } | ConvertTo-Json -Compress) `
                    -TimeoutSec 5 | Out-Null
            } catch {
                # Best effort: the tunnel works whether or not the dashboard
                # shows it.
            }

            Write-Host "(No access code needed for this URL - full control of this machine. Keep it to yourself.)"
        }
    }
}
finally {
    if (-not $script:IntentionalExit) {
        Invoke-Cleanup
    }
}
#endregion
