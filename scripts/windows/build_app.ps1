<#
.SYNOPSIS
    Build the Decurix Windows app from scratch.
.DESCRIPTION
    Clears what the last build staged and packaged, then builds everything
    again (scripts\windows\build-electron.ps1 does the building) and shows you
    the result. Two things come out, both under dist\electron, and either is
    run with a double click:

        Decurix Setup <version>.exe     the installer: Start menu and desktop
                                        shortcuts, an uninstaller
        win-unpacked\Decurix.exe        the same app, run where it is, with
                                        nothing installed

    The app needs nothing else on the computer - no Docker, no WSL, no Python.

    It does not touch your data: workers, their files, chats and settings live
    in %APPDATA%\decurix and %LOCALAPPDATA%\Decurix and are picked up by the
    new build. Installing over a running app closes it first. -Fresh is the
    exception: it deletes all of it, so the new build starts as on a computer
    that never had Decurix.

    Run it from PowerShell (.\scripts\windows\build_app.ps1), or right-click
    the file and choose "Run with PowerShell". Needs Go, Node.js with npm, and
    an internet connection the first time.
.PARAMETER MinFreeMemoryMB
    Free RAM the build insists on before it starts (default 512; it builds
    slowly when little is free, with Go and Node capped).
.PARAMETER Run
    Start the freshly built app (the unpacked one) when the build is done.
.PARAMETER NoOpen
    Do not show the result in Explorer.
.PARAMETER Fresh
    Also delete everything Decurix has kept on this computer, for good: every
    worker and its files, chats, task history, taught skills, settings and
    keys, paired devices, the profiles of apps copied into workers, and the
    local model server with the models it downloaded (over a gigabyte, fetched
    again when the app first needs them). Closes a running Decurix first, an
    installed one too.
#>
param(
    [int]$MinFreeMemoryMB = 512,
    [switch]$Run,
    [switch]$NoOpen,
    [switch]$Fresh
)

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$stage = Join-Path $repo 'desktop\electron\build'
$out = Join-Path $repo 'dist\electron'

# Every process of a running Decurix: the window and the control plane are
# both decurix.exe (the control plane in the app's resources folder), and the
# workers' agents and engines run from that same app folder. The control
# plane is detached and outlives the window, so both are looked for.
function Get-DecurixProcesses {
    $all = @(Get-CimInstance Win32_Process | Where-Object { $_.ExecutablePath })
    $roots = @($all | Where-Object { $_.Name -ieq 'decurix.exe' } | ForEach-Object {
        $dir = Split-Path -Parent $_.ExecutablePath
        if ((Split-Path -Leaf $dir) -ieq 'resources') { $dir = Split-Path -Parent $dir }
        $dir.TrimEnd('\') + '\'
    } | Select-Object -Unique)

    $found = @()
    foreach ($p in $all) {
        foreach ($root in $roots) {
            if ($p.ExecutablePath.StartsWith($root, [StringComparison]::OrdinalIgnoreCase)) { $found += $p; break }
        }
    }
    return $found
}

# Deletes what Decurix kept on this computer (the uninstaller's "also delete
# your workers ..." answer, without uninstalling). Nothing can be deleted
# under a running control plane, so Decurix is closed first; the workers'
# private desktops and what they opened end with it, a few seconds later,
# which is what the retries wait for.
function Clear-DecurixData {
    $data = Join-Path $env:APPDATA 'decurix'
    $store = Join-Path $env:LOCALAPPDATA 'Decurix'

    for ($try = 0; $try -lt 5; $try++) {
        $procs = @(Get-DecurixProcesses)
        if ($procs.Count -eq 0) { break }
        if ($try -eq 0) { Write-Host "Closing Decurix ($($procs.Count) processes)" }
        $procs | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
        Start-Sleep -Seconds 1
    }

    # The model server is started detached from the store and left running
    # between runs of the app. A person's own Ollama is elsewhere.
    Get-CimInstance Win32_Process | Where-Object { $_.ExecutablePath -and $_.ExecutablePath.StartsWith($store + '\', [StringComparison]::OrdinalIgnoreCase) } |
        ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }

    foreach ($target in @($data, $store)) {
        if (-not (Test-Path $target)) { continue }
        Write-Host "Deleting $target"
        for ($try = 0; (Test-Path $target) -and $try -lt 10; $try++) {
            if ($try -gt 0) { Start-Sleep -Seconds 2 }
            Remove-Item $target -Recurse -Force -ErrorAction SilentlyContinue
        }
        if (Test-Path $target) {
            throw "Could not delete $target - something still has a file in it open. Close it and run this again."
        }
    }
}

Write-Host '========================================'
Write-Host ' Building the Decurix app (Windows)'
Write-Host '========================================'

# The unpacked app cannot be replaced while it is running from that folder.
# (An installed copy runs from elsewhere and is left alone.)
#
# Closing the app's window does not end it: the control plane (and the workers
# it runs) is detached and stays, so after any run of the last build something
# is always still running from that folder. That part is stopped here - the
# workers that were running start again with the new build. An open window is
# somebody using the app, and is only closed for them by -Fresh, which says it
# closes a running Decurix.
$unpacked = Join-Path $out 'win-unpacked'
function Get-UnpackedProcesses {
    @(Get-CimInstance Win32_Process | Where-Object { $_.ExecutablePath -and $_.ExecutablePath.StartsWith($unpacked + '\', [StringComparison]::OrdinalIgnoreCase) })
}
$running = Get-UnpackedProcesses
if ($running.Count -gt 0) {
    $window = @($running | Where-Object { (Split-Path -Parent $_.ExecutablePath).TrimEnd('\') -ieq $unpacked })
    if ($window.Count -gt 0 -and -not $Fresh) {
        throw "The app built last time is open (from $unpacked). Close its window, then build again."
    }

    Write-Host "Stopping the Decurix the last build left running in the background ($($running.Count) processes)"
    for ($try = 0; $running.Count -gt 0 -and $try -lt 10; $try++) {
        $running | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
        Start-Sleep -Seconds 1
        $running = Get-UnpackedProcesses
    }
    if ($running.Count -gt 0) {
        throw "Could not stop what is running from $unpacked ($(($running | ForEach-Object { $_.Name } | Select-Object -Unique) -join ', ')). End it in Task Manager, then build again."
    }
}

# One build at a time. Two at once stage into the same folders and each
# deletes node_modules under the other: both then fail in "npm ci" with EPERM
# on a folder that differs from run to run. The lock is a file held open, so
# it is let go when this script ends however it ends, and when its window is
# closed.
$lockPath = Join-Path ([IO.Path]::GetTempPath()) 'decurix-build_app.lock'
try {
    $lock = [IO.File]::Open($lockPath, [IO.FileMode]::OpenOrCreate, [IO.FileAccess]::ReadWrite, [IO.FileShare]::None)
} catch [IO.IOException] {
    throw 'Another build of the Decurix app is already running (another terminal, or an assistant building for you). Wait for it to finish, or stop it, then build again.'
}

try {
    # From scratch: nothing an earlier build compiled, staged or packaged is
    # reused. The pinned downloads (Python, cloudflared, the wheels) are kept -
    # each is checked against its SHA-256 before it is used, so a kept one is as
    # good as a fresh one and the build does not need them again.
    Write-Host ''
    Write-Host '=== Clearing the last build ==='
    foreach ($old in @('runtime', 'stack', 'decurix.exe', 'decurix-android-arm64')) {
        $path = Join-Path $stage $old
        if (Test-Path $path) { Remove-Item $path -Recurse -Force }
    }
    if (Test-Path $out) { Remove-Item $out -Recurse -Force }

    if ($Fresh) {
        Write-Host ''
        Write-Host '=== Deleting everything Decurix kept ==='
        Clear-DecurixData
    }

    Write-Host ''
    Write-Host '=== Building ==='
    & (Join-Path $PSScriptRoot 'build-electron.ps1') -MinFreeMemoryMB $MinFreeMemoryMB
} finally {
    $lock.Dispose()
}

$installer = Get-ChildItem $out -Filter 'Decurix Setup *.exe' -ErrorAction SilentlyContinue | Select-Object -First 1
$app = Join-Path $unpacked 'Decurix.exe'
if (-not $installer -or -not (Test-Path $app)) {
    throw "The build finished without an installer and an app under $out."
}

Write-Host ''
Write-Host '========================================'
Write-Host ' The app is built'
Write-Host '========================================'
Write-Host ''
Write-Host "Installer:       $($installer.FullName)"
Write-Host "Run in place:    $app"
Write-Host ''
if ($Fresh) {
    Write-Host 'Double-click either one. It starts with no workers and no settings.'
} else {
    Write-Host 'Double-click either one. Your workers and settings are kept.'
}

if ($Run) {
    # Started from a shell that an Electron program opened (VS Code's
    # terminal), the app would inherit ELECTRON_RUN_AS_NODE and run as Node.
    Remove-Item Env:ELECTRON_RUN_AS_NODE -ErrorAction SilentlyContinue
    Start-Process $app
} elseif (-not $NoOpen) {
    Start-Process explorer.exe -ArgumentList "/select,`"$($installer.FullName)`""
}
