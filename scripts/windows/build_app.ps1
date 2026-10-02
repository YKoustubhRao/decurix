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
    new build. Installing over a running app closes it first.

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
#>
param(
    [int]$MinFreeMemoryMB = 512,
    [switch]$Run,
    [switch]$NoOpen
)

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$stage = Join-Path $repo 'desktop\electron\build'
$out = Join-Path $repo 'dist\electron'

Write-Host '========================================'
Write-Host ' Building the Decurix app (Windows)'
Write-Host '========================================'

# The unpacked app cannot be replaced while it is running from that folder.
# (An installed copy runs from elsewhere and is left alone.)
$unpacked = Join-Path $out 'win-unpacked'
$running = @(Get-CimInstance Win32_Process | Where-Object { $_.ExecutablePath -and $_.ExecutablePath.StartsWith($unpacked, [StringComparison]::OrdinalIgnoreCase) })
if ($running.Count -gt 0) {
    throw "The app built last time is running from $unpacked. Close it (and end 'decurix.exe' in Task Manager if it stays), then build again."
}

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

Write-Host ''
Write-Host '=== Building ==='
& (Join-Path $PSScriptRoot 'build-electron.ps1') -MinFreeMemoryMB $MinFreeMemoryMB

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
Write-Host 'Double-click either one. Your workers and settings are kept.'

if ($Run) {
    # Started from a shell that an Electron program opened (VS Code's
    # terminal), the app would inherit ELECTRON_RUN_AS_NODE and run as Node.
    Remove-Item Env:ELECTRON_RUN_AS_NODE -ErrorAction SilentlyContinue
    Start-Process $app
} elseif (-not $NoOpen) {
    Start-Process explorer.exe -ArgumentList "/select,`"$($installer.FullName)`""
}
