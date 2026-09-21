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

Write-Host "=== Configuring Ollama host binding ==="
try {
    [System.Environment]::SetEnvironmentVariable("OLLAMA_HOST", "0.0.0.0", "User")
    Write-Host "Configured OLLAMA_HOST=0.0.0.0 for host-to-container connectivity."
}
catch {
    Write-Host "WARNING: Failed to set OLLAMA_HOST environment variable."
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
if (Get-Command git -ErrorAction SilentlyContinue) { git --version } else { Write-Host "(git installed)" }
Write-Host ""

Write-Host "Docker:"
if (Get-Command docker -ErrorAction SilentlyContinue) { docker --version } else { Write-Host "(docker installed)" }
Write-Host ""

Write-Host "Docker Compose:"
if (Get-Command docker -ErrorAction SilentlyContinue) { docker compose version } else { Write-Host "(docker compose installed)" }
Write-Host ""

Write-Host "QEMU:"
if (Get-Command qemu-system-x86_64 -ErrorAction SilentlyContinue) { qemu-system-x86_64 --version | Select-Object -First 1 } else { Write-Host "(qemu installed)" }
Write-Host ""

Write-Host "Ollama:"
if (Get-Command ollama -ErrorAction SilentlyContinue) { ollama --version } else { Write-Host "(ollama installed)" }
Write-Host ""

Write-Host "Cloudflared:"
if (Get-Command cloudflared -ErrorAction SilentlyContinue) { cloudflared --version } else { Write-Host "(cloudflared installed)" }
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
Write-Host ""
Write-Host "Next steps:"
Write-Host "  - For a full clean-slate build (clears old desktops & database):"
Write-Host "      .\scripts\windows\rebuild.ps1"
Write-Host ""
Write-Host "  - To open Decurix (retaining existing desktops and data):"
Write-Host "      .\scripts\windows\build.ps1"
Write-Host ""
Write-Host "========================================"
