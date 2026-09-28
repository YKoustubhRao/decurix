#!/bin/bash
#
# Shared by scripts/linux/build.sh and scripts/linux/rebuild.sh. Meant to be
# sourced, not executed: it defines decurix_lan_ip, the address another
# device on this network (a phone on the same Wi-Fi) should use to reach the
# access-code-gated listener.
#
# On an ordinary Linux machine that is simply the machine's own address. Under
# WSL2 in its default NAT mode it is not: WSL has a private address (172.x)
# that nothing but Windows can reach, so a phone given it gets nowhere. There
# the script forwards Windows' own LAN address, on the gated port only, into
# WSL (netsh portproxy + one firewall rule) and returns Windows' address.
# That needs administrator rights, so Windows shows one UAC prompt - and only
# again when either address has changed since. The primary (admin) listener
# is never forwarded. DECURIX_WSL_LAN_FORWARD=0 in .env skips all of this.

# linux_lan_ip is this Linux system's own outward address: "ip route get"
# asks the kernel which source address it would use to reach the internet,
# without sending a packet; hostname -I is the fallback.
linux_lan_ip() {
    local ip
    ip="$(ip route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p')"
    if [ -z "$ip" ]; then
        ip="$(hostname -I 2>/dev/null | awk '{print $1}')"
    fi
    echo "$ip"
}

# windows_ps runs a PowerShell command on the Windows side of WSL, output with
# Windows line endings removed.
windows_ps() {
    powershell.exe -NoProfile -NonInteractive -Command "$1" 2>/dev/null | tr -d '\r'
}

# decurix_lan_ip PORT prints the address other devices should use for PORT.
# Progress and warnings go to stderr, so callers can capture the address.
decurix_lan_ip() {
    local port="$1"
    local own
    own="$(linux_lan_ip)"

    if ! grep -qi microsoft /proc/version 2>/dev/null || ! command -v powershell.exe >/dev/null 2>&1; then
        echo "$own"
        return 0
    fi

    local skip
    skip="$(read_env_var DECURIX_WSL_LAN_FORWARD 2>/dev/null || true)"
    if [ "$skip" = "0" ]; then
        echo "$own"
        return 0
    fi

    local win
    win="$(windows_ps '(Get-NetIPConfiguration | Where-Object { $_.IPv4DefaultGateway -ne $null -and $_.NetAdapter.Status -eq "Up" } | Select-Object -First 1).IPv4Address.IPAddress' | head -n1)"

    # No Windows address found, or WSL's mirrored networking (it shares
    # Windows' addresses already): nothing to forward.
    if [ -z "$win" ] || [ "$win" = "$own" ]; then
        echo "${win:-$own}"
        return 0
    fi

    if windows_ps 'netsh interface portproxy show v4tov4' \
        | grep -qE "^${win//./\\.}[[:space:]]+${port}[[:space:]]+${own//./\\.}[[:space:]]+${port}([[:space:]]|$)"; then
        echo "$win"
        return 0
    fi

    echo "" >&2
    echo "Phones on your Wi-Fi cannot reach WSL directly. Asking Windows to forward" >&2
    echo "$win:$port (the code-gated port only) into WSL - approve the UAC prompt." >&2

    local ps
    ps="netsh interface portproxy delete v4tov4 listenaddress=$win listenport=$port 2>\$null | Out-Null
netsh interface portproxy add v4tov4 listenaddress=$win listenport=$port connectaddress=$own connectport=$port | Out-Null
if (-not (Get-NetFirewallRule -DisplayName 'Decurix LAN access ($port)' -ErrorAction SilentlyContinue)) {
    New-NetFirewallRule -DisplayName 'Decurix LAN access ($port)' -Direction Inbound -Protocol TCP -LocalPort $port -Action Allow -Profile Any | Out-Null
}"

    # -EncodedCommand takes UTF-16LE base64: no quoting survives the hop into
    # an elevated PowerShell otherwise.
    local encoded
    encoded="$(printf '%s' "$ps" | iconv -f UTF-8 -t UTF-16LE | base64 -w0)"
    windows_ps "try { Start-Process powershell -Verb RunAs -Wait -WindowStyle Hidden -ArgumentList '-NoProfile','-EncodedCommand','$encoded' } catch { exit 1 }" >/dev/null || true

    if windows_ps 'netsh interface portproxy show v4tov4' \
        | grep -qE "^${win//./\\.}[[:space:]]+${port}[[:space:]]+${own//./\\.}[[:space:]]+${port}([[:space:]]|$)"; then
        echo "Forwarded. Phones on this Wi-Fi can use http://$win:$port" >&2
        echo "$win"
        return 0
    fi

    echo "The forward was not set up (prompt declined?). The local-network link will" >&2
    echo "only work from this computer; the tunnel link works from anywhere." >&2
    echo "$own"
}
