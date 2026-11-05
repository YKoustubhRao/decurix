<#
.SYNOPSIS
    Decurix Setup Script for Windows
.DESCRIPTION
    Installs and configures required components for Decurix on Windows.
    This includes Docker, QEMU, Git, Ollama, Cloudflare Tunnel, and other dependencies.
.NOTES
    Requires administrative privileges.
#>

#Requires -RunAsAdministrator

Write-Host "========================================"
Write-Host "        Decurix Setup"
Write-Host "========================================"
Write-Host ""

$ErrorActionPreference = "Stop"

# Get the real user who initiated the script
$REAL_USER = if ($env:SUDO_USER) { $env:SUDO_USER } else { $env:USERNAME }

Write-Host "=== Updating package manager ==="
winget source update -q
Write-Host ""

Write-Host "=== Installing base dependencies ==="
$packages = @(
    "Git.Git",
    "Docker.DockerDesktop",
    "qemu.qemu",
    "jqlang.jq",
    "7zip.7zip",
    "OpenSSH.OpenSSH",
    "Ollama.Ollama"
)

# winget says whether it worked through its exit code, not an exception: a
# failure is collected and said at the end, never reported as installed.
$installFailures = @()
# winget's "already installed" and "no newer version" answers are not failures.
$wingetFine = @(0, -1978335189, -1978335135)  # 0, 0x8A15002B (no applicable update), 0x8A150061 (already installed)
foreach ($package in $packages) {
    try {
        Write-Host "Installing $package..."
        winget install --id $package --silent --accept-package-agreements --accept-source-agreements
        if ($wingetFine -notcontains $LASTEXITCODE) {
            Write-Host "WARNING: winget could not install $package (exit $LASTEXITCODE)."
            $installFailures += $package
        }
    }
    catch {
        Write-Host "WARNING: Failed to install $package - $($_.Exception.Message)"
        $installFailures += $package
    }
}
Write-Host ""

Write-Host "=== Configuring Ollama host binding ==="
# Nothing to change: Docker Desktop forwards host.docker.internal to this
# computer's 127.0.0.1, where Ollama listens by default. Ollama has no login,
# so it is never put on every address. An earlier version of this script did
# that; it is taken back here.
if ([System.Environment]::GetEnvironmentVariable("OLLAMA_HOST", "User") -eq "0.0.0.0") {
    [System.Environment]::SetEnvironmentVariable("OLLAMA_HOST", $null, "User")
    Write-Host "Removed OLLAMA_HOST set to 0.0.0.0 for your user (an earlier Decurix set it); restart Ollama."
}
Write-Host "Ollama stays on 127.0.0.1; desktops reach it through Docker Desktop."
Write-Host ""

Write-Host "=== Installing Docker ==="
if (Get-Command docker -ErrorAction SilentlyContinue) {
    Write-Host "Docker already installed."
} else {
    Write-Host "Installing Docker..."
    try {
        winget install --id Docker.DockerDesktop --silent --accept-package-agreements --accept-source-agreements
    }
    catch {
        Write-Host "WARNING: Docker installation failed - $($_.Exception.Message)"
    }
}
Write-Host ""

Write-Host "=== Enabling Docker ==="
# Docker Desktop for Windows handles its own service management
Write-Host "Docker Desktop service management is handled by the application itself."
Write-Host ""

Write-Host "=== Configuring Docker for $REAL_USER ==="
try {
    $user = Get-LocalUser -Name $REAL_USER -ErrorAction SilentlyContinue
    if ($user) {
        Write-Host "Adding $REAL_USER to the docker-users group..."
        Add-LocalGroupMember -Group "docker-users" -Member $REAL_USER -ErrorAction Stop
    } else {
        Write-Host "WARNING: Could not find user $REAL_USER"
    }
}
catch {
    Write-Host "WARNING: Failed to add user to Docker group - $($_.Exception.Message)"
}
Write-Host ""

Write-Host "=== Installing Cloudflare cloudflared ==="
if (Get-Command cloudflared -ErrorAction SilentlyContinue) {
    Write-Host "cloudflared already installed."
} else {
    Write-Host "Installing cloudflared..."
    try {
        $tempDir = [System.IO.Path]::GetTempPath()
        $installerPath = Join-Path $tempDir "cloudflared.msi"
        $url = "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-windows-amd64.msi"

        Write-Host "Downloading cloudflared installer..."
        Invoke-WebRequest -Uri $url -OutFile $installerPath

        Write-Host "Installing cloudflared..."
        Start-Process msiexec.exe -Wait -ArgumentList "/i `"$installerPath`" /qn"

        Remove-Item $installerPath -ErrorAction SilentlyContinue
    }
    catch {
        Write-Host "WARNING: Failed to install cloudflared - $($_.Exception.Message)"
    }
}
Write-Host ""

Write-Host "========================================"
Write-Host "        Verifying Installation"
Write-Host "========================================"
Write-Host ""

# Each tool is looked for, not assumed. A tool installed a moment ago may not
# be on this window's PATH yet, so "not found" says where to look again.
$notFound = @()
function Show-Tool([string]$Label, [string]$Command, [string[]]$Arguments) {
    Write-Host "${Label}:"
    if (Get-Command $Command -ErrorAction SilentlyContinue) {
        & $Command @Arguments 2>$null | Select-Object -First 1
    } else {
        Write-Host "  NOT FOUND in this window (one installed just now shows up in a new window)"
        $script:notFound += $Label
    }
    Write-Host ""
}
Show-Tool 'Git' 'git' @('--version')
Show-Tool 'Docker' 'docker' @('--version')
Show-Tool 'Docker Compose' 'docker' @('compose', 'version')
Show-Tool 'QEMU' 'qemu-system-x86_64' @('--version')
Show-Tool 'Ollama' 'ollama' @('--version')
Show-Tool 'Cloudflared' 'cloudflared' @('--version')

if ($installFailures.Count -gt 0) {
    Write-Host "========================================"
    Write-Host "        Setup INCOMPLETE"
    Write-Host "========================================"
    Write-Host "These could not be installed: $($installFailures -join ', '). Install them yourself, then re-run."
    exit 1
}
if ($notFound.Count -gt 0) {
    Write-Host "Not found in this window: $($notFound -join ', '). Open a new PowerShell window to check again."
    Write-Host ""
}

Write-Host "========================================"
Write-Host "        Setup Complete"
Write-Host "========================================"
Write-Host ""

Write-Host "IMPORTANT:"
Write-Host "'docker-users' group membership was updated for:"
Write-Host "  $REAL_USER"
Write-Host ""
Write-Host "Sign out of Windows and back in (Docker Desktop may ask for a"
Write-Host "restart), start Docker Desktop, and then in a NEW PowerShell"
Write-Host "window - not this Administrator one - run:"
Write-Host "  Set-ExecutionPolicy -Scope Process Bypass"
Write-Host ""
Write-Host "Next steps:"
Write-Host "  - For a full clean-slate build (clears old desktops & database):"
Write-Host "      .\scripts\windows\rebuild.ps1"
Write-Host ""
Write-Host "  - To open Decurix (retaining existing desktops and data):"
Write-Host "      .\scripts\windows\build.ps1"
Write-Host ""
Write-Host "========================================"
