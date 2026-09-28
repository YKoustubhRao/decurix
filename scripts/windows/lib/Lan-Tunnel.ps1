<#
.SYNOPSIS
    Shared by scripts/windows/build.ps1 and scripts/windows/rebuild.ps1.
.DESCRIPTION
    Dot-sourced, not executed directly: defines Start-LanTunnel/
    Stop-LanTunnel, a second Cloudflare quick tunnel pointed at the
    access-code-gated listener (rather than the primary, unauthenticated one
    both scripts already tunnel in their own foreground loop) - so the gated
    URL is reachable from anywhere, not only this machine's own network like
    the plain LAN address is. Both scripts already print and report that LAN
    address; this gives the same gated listener a link that works
    off-network too, still behind the code.

    Requires: cloudflared already confirmed installed by the caller - this
    file has no error message of its own for a missing binary, since the
    caller's own primary-tunnel section already has one and exits before
    this would ever be reached without it.
#>

# Start-LanTunnel opens a Cloudflare tunnel to 127.0.0.1:$LanPort as a real
# background process (Start-Process, not Start-Job - a job's own PowerShell
# host process is not a reliable way to also kill the native cloudflared.exe
# it launches) so the caller's own primary tunnel below can still run in the
# foreground, and reports its trycloudflare.com URL to the control plane as
# lan_tunnel_url once cloudflared prints one - polling its redirected output
# for up to 30 seconds, the same order of magnitude the primary tunnel
# usually takes to come up. Prints the URL, or a one-line note if it didn't
# show up in time (the dashboard's Mobile menu will still pick it up
# whenever it does). Sets $script:LanTunnelProcess/$script:LanTunnelLogFiles
# for Stop-LanTunnel to clean up.
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

function Start-LanTunnel {
    param([string]$LanPort)

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

    $outLog = [System.IO.Path]::GetTempFileName()
    $errLog = [System.IO.Path]::GetTempFileName()
    $script:LanTunnelLogFiles = @($outLog, $errLog)

    $script:LanTunnelProcess = Start-Process -FilePath "cloudflared" `
        -ArgumentList @("tunnel", "--protocol", "http2", "--url", "http://127.0.0.1:$LanPort") `
        -RedirectStandardOutput $outLog -RedirectStandardError $errLog `
        -NoNewWindow -PassThru

    $lanTunnelUrl = $null
    for ($i = 1; $i -le 30; $i++) {
        Start-Sleep -Seconds 1

        $content = (Get-Content $outLog -Raw -ErrorAction SilentlyContinue) +
                   (Get-Content $errLog -Raw -ErrorAction SilentlyContinue)
        # A quick tunnel's address is hyphenated words; cloudflared's own errors
        # mention https://api.trycloudflare.com, which is Cloudflare's API, not a tunnel.
        if ($content -match 'https://[a-z0-9]+(-[a-z0-9]+)+\.trycloudflare\.com') {
            $lanTunnelUrl = $Matches[0]
            break
        }
    }

    if ($lanTunnelUrl) {
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
    } else {
        Write-Host "(It didn't come up within 30s - it'll still appear in the dashboard's Mobile menu once it does.)"
    }
}

# Stop-LanTunnel kills the background cloudflared process Start-LanTunnel
# started, if any, and removes its log files - called from each script's own
# Invoke-Cleanup on exit/interrupt so it never outlives the script as an
# orphaned process.
function Stop-LanTunnel {
    if ($script:LanTunnelProcess -and -not $script:LanTunnelProcess.HasExited) {
        Stop-Process -Id $script:LanTunnelProcess.Id -Force -ErrorAction SilentlyContinue
    }
    if ($script:LanTunnelLogFiles) {
        Remove-Item $script:LanTunnelLogFiles -ErrorAction SilentlyContinue
    }
}
