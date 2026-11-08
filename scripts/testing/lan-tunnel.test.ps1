# Checks the restart loop of the public link's quick tunnel in
# scripts/windows/lib/Lan-Tunnel.ps1 (B-29): a tunnel that exits is started
# again after 1 s, then 2 s, up to the cap, and each start's new address is
# reported; the loop stops when the public link is turned off or the script
# stops it, and never starts the tunnel while the port is not the access-code
# page. A stub stands in for cloudflared, nothing reaches a real control plane,
# and the waits are the real ones (a few seconds). Runs in Windows
# PowerShell 5.1 and in pwsh on Windows:
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File scripts/testing/lan-tunnel.test.ps1

$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot '../windows/lib/Lan-Tunnel.ps1')

$failures = 0
function Check([string]$Name, $Got, $Want) {
    if ("$Got" -ceq "$Want") { Write-Output "ok   $Name" }
    else { Write-Output "FAIL ${Name}: got [$Got], want [$Want]"; $script:failures++ }
}

if (-not $IsWindows -and $PSVersionTable.PSEdition -eq 'Core') {
    Write-Output "skip: the stub tunnel is a .cmd file"
    exit 0
}

# What the loop checks before a start, and what it would say and send, answered here instead.
$script:linkOn = $true
$script:gated = $true
$script:said = New-Object System.Collections.Generic.List[string]
$script:posted = New-Object System.Collections.Generic.List[string]
function Test-PublicLinkEnabled { return $script:linkOn }
function Test-GatedPort { param([string]$Port) return $script:gated }
function Get-NamedTunnelUrl { return $null }
function Write-Host { param([string]$Object) $script:said.Add($Object) }
function Invoke-RestMethod {
    param([string]$Method, [string]$Uri, [string]$ContentType, [string]$Body, [int]$TimeoutSec)
    if ($Uri -like '*/api/tunnel') { $script:posted.Add($Body) }
    return $null
}

$dir = Join-Path ([System.IO.Path]::GetTempPath()) ('decurix-tunnel-test-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $dir | Out-Null
try {
    # Each start prints a new quick-tunnel address, stays up about a second and exits with code 3.
    $stub = Join-Path $dir 'stub.cmd'
    [System.IO.File]::WriteAllText($stub, "@echo INF https://stub-run-%RANDOM%%RANDOM%.trycloudflare.com`r`n@ping -n 2 127.0.0.1 >nul`r`n@exit 3`r`n")
    $script:LanTunnelCommand = $stub
    $script:LanTunnelMaxDelay = 2

    function Step-For([double]$Seconds) {
        $until = (Get-Date).AddSeconds($Seconds)
        while ((Get-Date) -lt $until) {
            Step-LanTunnel -LanPort '8090'
            Start-Sleep -Milliseconds 100
        }
    }
    function Restarts { @($script:said | Where-Object { $_ -like 'Public link: the tunnel exited*' }) }

    # 1. Exits are restarted with a doubling, capped wait, and every start's address is reported.
    Start-LanTunnel -LanPort '8090'
    Step-For 6 # up 0-1 s, waits 1 s, up 2-3 s, waits 2 s, up 5-6 s
    $exits = Restarts
    Check 'first exit logged with its code and a 1 s wait' ($exits[0] -like '*exited with code 3 after *s; starting it again in 1s.') $true
    Check 'the wait doubles' ($exits[1] -like '*starting it again in 2s.') $true
    Check 'the wait is capped' (@($exits | Where-Object { $_ -like '*in 4s*' }).Count) 0
    Check 'started again' (@($script:said | Where-Object { $_ -eq 'Public link: tunnel started again.' }).Count -ge 2) $true
    Check 'each start reports its own address' (@($script:posted | Select-Object -Unique).Count -ge 2) $true

    # 2. Turned off: no more starts.
    $script:linkOn = $false
    Step-For 3
    Check 'says it is turned off' ($script:said -contains 'Public link: turned off - the tunnel is not started again.') $true
    Check 'the loop has let the tunnel go' $script:LanTunnelWanted $false
    $n = (Restarts).Count
    Step-For 1
    Check 'no start after it was turned off' (Restarts).Count $n

    # 3. A port that is not the access-code page is never tunnelled; the loop waits and checks again.
    $script:said.Clear()
    $script:linkOn = $true
    $script:gated = $false
    $script:LanTunnelWanted = $true
    $script:LanTunnelProcess = $null
    $script:LanTunnelDelay = 1
    $script:LanTunnelRestartAt = Get-Date
    Step-For 2.5
    Check 'says the port is not the gate' (@($script:said | Where-Object { $_ -like '*is not serving the access-code page; trying again in*' }).Count -ge 2) $true
    Check 'never started while the gate is closed' $script:LanTunnelProcess $null

    # 4. Stop-LanTunnel ends the loop: Watch-LanTunnel returns instead of waiting.
    Stop-LanTunnel
    $watch = Measure-Command { Watch-LanTunnel -LanPort '8090' }
    Check 'a stopped loop does not wait' ($watch.TotalSeconds -lt 1) $true
} finally {
    Stop-LanTunnel
    Remove-Item -LiteralPath $dir -Recurse -Force -ErrorAction SilentlyContinue
}

if ($failures) {
    Write-Output "$failures check(s) failed"
    exit 1
}
Write-Output 'all checks passed'
