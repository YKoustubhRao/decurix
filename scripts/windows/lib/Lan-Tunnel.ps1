<#
.SYNOPSIS
    Shared by scripts/windows/build.ps1 and scripts/windows/rebuild.ps1.
.DESCRIPTION
    Dot-sourced, not executed directly: defines Show-LanAccess (the LAN
    address and access code), and Start-LanTunnel/Stop-LanTunnel, a
    Cloudflare quick tunnel pointed at the access-code-gated listener (the
    only listener either script tunnels: the primary, unauthenticated one is
    never exposed) - so the gated URL is reachable from anywhere, not only
    this machine's own network like the plain LAN address is. Both scripts already print and report that LAN
    address; this gives the same gated listener a link that works
    off-network too, still behind the code.

    Requires: cloudflared already confirmed installed by the caller - this
    file has no error message of its own for a missing binary, since the
    caller's own tunnel section already has one and exits before this would
    ever be reached without it.
#>

# Start-LanTunnel opens a Cloudflare tunnel to 127.0.0.1:$LanPort as a
# background process (Start-LanTunnelProcess) so the caller can go on in the
# foreground, and reports its trycloudflare.com URL to the control plane as
# lan_tunnel_url once cloudflared prints one - polling its redirected output
# for up to 30 seconds, the same order of magnitude a quick tunnel
# usually takes to come up. Prints the URL, or a one-line note if it didn't
# show up in time (Watch-LanTunnel reports it whenever it does). The caller
# then runs Watch-LanTunnel, which restarts the tunnel if it exits.
# Sets $script:LanTunnelProcess/$script:LanTunnelLogFiles for Stop-LanTunnel
# to clean up.
# Get-NamedTunnelUrl returns the fixed link on the person's own domain when
# one is set up in the dashboard (Mobile menu > Your own link), or $null.
# Decurix runs that tunnel's connector itself (internal/cftunnel), so a
# quick tunnel would only add a second, changing address.
function Get-NamedTunnelUrl {
    try {
        $status = Invoke-RestMethod -Uri "http://127.0.0.1:8080/api/cftunnel" -TimeoutSec 5
        if ($status.configured -and $status.url) { return $status.url }
    } catch {
        # Not reachable or an older control plane: fall back to a quick tunnel.
    }
    return $null
}

# Test-PublicLinkEnabled says whether the person turned the public link on: DECURIX_PUBLIC_LINK=1 in the environment
# or in .env. It is off by default (owner decision O-5), as in the app.
function Test-PublicLinkEnabled {
    if ($env:DECURIX_PUBLIC_LINK -eq "1") { return $true }
    if (Test-Path ".env") {
        foreach ($line in [System.IO.File]::ReadAllLines((Resolve-Path ".env"), [System.Text.Encoding]::UTF8)) {
            if ($line -match '^\s*DECURIX_PUBLIC_LINK\s*=\s*"?1"?\s*$') { return $true }
        }
    }
    return $false
}

# Test-GatedPort says whether port is the gated listener's: never the primary one (8080), and serving the access-code
# page right now (the same check the app makes before it exposes anything, B-07).
function Test-GatedPort {
    param([string]$Port)
    if ($Port -eq "8080") { return $false }
    try {
        $page = Invoke-WebRequest -Uri "http://127.0.0.1:$Port/" -UseBasicParsing -TimeoutSec 5
        return $page.Content -like "*Enter the access code*"
    } catch {
        return $false
    }
}

function Start-LanTunnel {
    param([string]$LanPort)

    if (-not (Test-PublicLinkEnabled)) {
        Write-Host ""
        Write-Host "Public link: off. Phones reach Decurix on this network only. For a link that works from anywhere (it still needs the access code), set DECURIX_PUBLIC_LINK=1 in .env and run this again."
        return
    }
    if (-not (Test-GatedPort -Port $LanPort)) {
        Write-Host ""
        Write-Host "Public link: not started - 127.0.0.1:$LanPort is not serving the access-code page, and only that listener is ever tunnelled."
        return
    }

    $namedUrl = Get-NamedTunnelUrl
    if ($namedUrl) {
        Write-Host ""
        Write-Host "=== Your own link is set up ==="
        Write-Host "Gated URL (needs the access code, works from anywhere): $namedUrl"
        Write-Host "(Decurix keeps its tunnel running itself - no quick tunnel is opened.)"
        return
    }

    Write-Host ""
    Write-Host "=== Starting a second tunnel for the access-code-gated URL ==="

    $script:LanTunnelWanted = $true
    $script:LanTunnelDelay = $script:LanTunnelFirstDelay
    Start-LanTunnelProcess -LanPort $LanPort

    for ($i = 1; $i -le 30; $i++) {
        Start-Sleep -Seconds 1
        if (Update-LanTunnelUrl) { return }
    }
    Write-Host "(It didn't come up within 30s - it'll still appear in the dashboard's Mobile menu once it does.)"
}

# The restart loop's timings (B-29): the first wait after the tunnel exits is LanTunnelFirstDelay seconds, doubling up
# to LanTunnelMaxDelay; a tunnel that stayed up LanTunnelResetAfter seconds starts again from the first wait. Variables
# so scripts/testing/lan-tunnel.test.ps1 can shorten them and stand a stub in for cloudflared.
$script:LanTunnelCommand = "cloudflared"
$script:LanTunnelFirstDelay = 1
$script:LanTunnelMaxDelay = 60
$script:LanTunnelResetAfter = 600

# Start-LanTunnelProcess starts cloudflared as a real background process
# (Start-Process, not Start-Job - a job's own PowerShell host process is not a
# reliable way to also kill the native cloudflared.exe it launches), with its
# output in fresh temporary files that Update-LanTunnelUrl reads.
function Start-LanTunnelProcess {
    param([string]$LanPort)

    if ($script:LanTunnelLogFiles) {
        Remove-Item $script:LanTunnelLogFiles -ErrorAction SilentlyContinue
    }
    $outLog = [System.IO.Path]::GetTempFileName()
    $errLog = [System.IO.Path]::GetTempFileName()
    $script:LanTunnelLogFiles = @($outLog, $errLog)

    $script:LanTunnelProcess = Start-Process -FilePath $script:LanTunnelCommand `
        -ArgumentList @("tunnel", "--protocol", "http2", "--url", "http://127.0.0.1:$LanPort") `
        -RedirectStandardOutput $outLog -RedirectStandardError $errLog `
        -NoNewWindow -PassThru
    # Windows PowerShell only keeps a started process's exit code if its handle was opened while it ran.
    $null = $script:LanTunnelProcess.Handle
    $script:LanTunnelStartedAt = Get-Date
    $script:LanTunnelUrlPending = $true
}

# Update-LanTunnelUrl looks once, without waiting, for the address the running
# cloudflared printed and, the first time it shows, reports it to the control
# plane as lan_tunnel_url and prints it with the one-click pairing link. A
# restarted quick tunnel gets a new address, so each start reports its own.
# Returns whether the address is known.
function Update-LanTunnelUrl {
    if (-not $script:LanTunnelUrlPending) { return $true }
    if (-not $script:LanTunnelLogFiles) { return $false }
    $content = ""
    foreach ($file in $script:LanTunnelLogFiles) {
        $content += [string](Get-Content $file -Raw -ErrorAction SilentlyContinue)
    }
    # A quick tunnel's address is hyphenated words; cloudflared's own errors
    # mention https://api.trycloudflare.com, which is Cloudflare's API, not a tunnel.
    if ($content -notmatch 'https://[a-z0-9]+(-[a-z0-9]+)+\.trycloudflare\.com') { return $false }
    $lanTunnelUrl = $Matches[0]
    $script:LanTunnelUrlPending = $false

    try {
        Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:8080/api/tunnel" `
            -ContentType "application/json" `
            -Body (@{ lan_tunnel_url = $lanTunnelUrl } | ConvertTo-Json -Compress) `
            -TimeoutSec 5 | Out-Null
    } catch {
        # Best effort - the dashboard's Mobile menu is the fallback.
    }

    Write-Host "Gated tunnel URL (needs the access code, works from anywhere): $lanTunnelUrl"

    # One link that signs a device in and remembers it - what to open on
    # a phone or another computer (the dashboard's Mobile menu shows it too).
    try {
        $lan = Invoke-RestMethod -Uri "http://127.0.0.1:8080/api/lan-access/status" -TimeoutSec 5
        if ($lan.enabled -and $lan.code) {
            Write-Host "Open this on your phone or other computer (signs in, nothing to type):"
            Write-Host "  $lanTunnelUrl/__decurix_access/authenticate?code=$($lan.code)&remember=1&next=%2F"
        }
    } catch {
        # Best effort - the dashboard's Mobile menu shows the same link.
    }
    return $true
}

# Step-LanTunnel is one turn of the tunnel's restart loop (B-29), called about
# once a second by Watch-LanTunnel; it never waits itself. When cloudflared has
# exited it logs the exit code and schedules a restart: after 1 s, doubling to
# a 60 s cap, back to 1 s once a run has stayed up 10 minutes. When the time
# comes it checks again everything Start-LanTunnel checked - the public link is
# still on (DECURIX_PUBLIC_LINK, .env read again), no fixed link has been set up
# since, and the port still serves the access-code page, the only listener
# ever tunnelled - and only then starts it. Turned off, or replaced by a fixed
# link: no more restarts. Stop-LanTunnel ends the loop.
function Step-LanTunnel {
    param([string]$LanPort)

    if ($script:LanTunnelStopping -or -not $script:LanTunnelWanted) { return }
    $p = $script:LanTunnelProcess
    if ($p) {
        if (-not $p.HasExited) {
            [void](Update-LanTunnelUrl)
            return
        }
        $upFor = ((Get-Date) - $script:LanTunnelStartedAt).TotalSeconds
        if ($upFor -ge $script:LanTunnelResetAfter) { $script:LanTunnelDelay = $script:LanTunnelFirstDelay }
        $script:LanTunnelProcess = $null
        Write-Host "Public link: the tunnel exited with code $($p.ExitCode) after $([int]$upFor)s; starting it again in $($script:LanTunnelDelay)s."
        Set-LanTunnelRetry
        return
    }
    if ((Get-Date) -lt $script:LanTunnelRestartAt) { return }

    if (-not (Test-PublicLinkEnabled)) {
        Write-Host "Public link: turned off - the tunnel is not started again."
        $script:LanTunnelWanted = $false
        return
    }
    $namedUrl = Get-NamedTunnelUrl
    if ($namedUrl) {
        Write-Host "Public link: your own link is set up ($namedUrl) - the quick tunnel is not started again."
        $script:LanTunnelWanted = $false
        return
    }
    if (-not (Test-GatedPort -Port $LanPort)) {
        Write-Host "Public link: 127.0.0.1:$LanPort is not serving the access-code page; trying again in $($script:LanTunnelDelay)s."
        Set-LanTunnelRetry
        return
    }
    try {
        Start-LanTunnelProcess -LanPort $LanPort
        Write-Host "Public link: tunnel started again."
    } catch {
        Write-Host "Public link: could not start cloudflared ($($_.Exception.Message)); trying again in $($script:LanTunnelDelay)s."
        Set-LanTunnelRetry
    }
}

# Set-LanTunnelRetry schedules the next start after the current wait, and
# doubles the wait for the time after that, up to the cap.
function Set-LanTunnelRetry {
    $script:LanTunnelRestartAt = (Get-Date).AddSeconds($script:LanTunnelDelay)
    $script:LanTunnelDelay = [Math]::Min($script:LanTunnelDelay * 2, $script:LanTunnelMaxDelay)
}

# Watch-LanTunnel keeps the calling script in the foreground - and so its
# Ctrl+C handling - for as long as it runs, restarting the tunnel when it exits
# (Step-LanTunnel). With no tunnel to keep (the public link is off, or a fixed
# link is set up) it only waits.
function Watch-LanTunnel {
    param([string]$LanPort)

    while (-not $script:LanTunnelStopping) {
        Start-Sleep -Seconds 1
        Step-LanTunnel -LanPort $LanPort
    }
}

# Show-LanAccess reports this machine's LAN address on the gated port to the
# control plane and prints it, with the access code.
function Show-LanAccess {
    param([string]$LanPort)

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
}

# Stop-LanTunnel ends the restart loop, kills the background cloudflared
# process Start-LanTunnel started, if any, and removes its log files - called
# from each script's own Invoke-Cleanup on exit/interrupt so it never outlives
# the script as an orphaned process.
function Stop-LanTunnel {
    $script:LanTunnelStopping = $true
    if ($script:LanTunnelProcess -and -not $script:LanTunnelProcess.HasExited) {
        Stop-Process -Id $script:LanTunnelProcess.Id -Force -ErrorAction SilentlyContinue
    }
    if ($script:LanTunnelLogFiles) {
        Remove-Item $script:LanTunnelLogFiles -ErrorAction SilentlyContinue
    }
}
