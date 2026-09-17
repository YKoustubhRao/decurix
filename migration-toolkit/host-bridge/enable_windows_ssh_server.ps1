# Run this in PowerShell as Administrator on your Windows machine (once):
# Installs and starts the built-in Windows OpenSSH Server

Write-Host "Checking OpenSSH Server on Windows..." -ForegroundColor Cyan
Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0

Write-Host "Starting sshd service..." -ForegroundColor Cyan
Start-Service sshd
Set-Service -Name sshd -StartupType 'Automatic'

# Confirm firewall rule
if (!(Get-NetFirewallRule -Name "OpenSSH-Server-In-TCP" -ErrorAction SilentlyContinue)) {
    New-NetFirewallRule -Name 'OpenSSH-Server-In-TCP' -DisplayName 'OpenSSH Server (sshd)' -Enabled True -Direction Inbound -Protocol TCP -Action Allow -LocalPort 22
}

Write-Host "Windows OpenSSH Server is active and ready for Linux VM bridging!" -ForegroundColor Green
