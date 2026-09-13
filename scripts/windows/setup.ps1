<#
.SYNOPSIS
    Flowera Setup Script for Windows
.DESCRIPTION
    Installs and configures required components for Flowera on Windows.
    This includes Docker, QEMU, Git, Cloudflare Tunnel, and other dependencies.
.NOTES
    Requires administrative privileges.
#>

#Requires -RunAsAdministrator

Write-Host "========================================"
Write-Host "        Flowera Setup"
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
    "OpenSSH.OpenSSH"
)

foreach ($package in $packages) {
    try {
        Write-Host "Installing $package..."
        winget install --id $package --silent --accept-package-agreements --accept-source-agreements
    }
    catch {
        Write-Host "WARNING: Failed to install $package - $($_.Exception.Message)"
    }
}
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

Write-Host "Git:"
git --version
Write-Host ""

Write-Host "Docker:"
docker --version
Write-Host ""

Write-Host "Docker Compose:"
docker compose version
Write-Host ""

Write-Host "QEMU:"
qemu-system-x86_64 --version | Select-Object -First 1
Write-Host ""

Write-Host "Cloudflared:"
cloudflared --version
Write-Host ""

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
Write-Host "  .\scripts\windows\build.ps1"
Write-Host ""
Write-Host "========================================"
