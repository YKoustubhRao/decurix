# Called by the uninstaller and build_app.ps1 -Fresh for a shared WSL runtime.
#
# Only what Decurix created there is removed: windows-runtime.json lists what
# the engine already held when the app began using it (main.js, `inventory`),
# and maintain-existing.sh keeps those. A record from before that list existed
# cannot tell the two apart, so nothing is removed and the exit code is 3.
param([ValidateSet('stop', 'reset', 'delete')][string]$Action = 'stop')
$ErrorActionPreference = 'Stop'
$record = Join-Path $env:APPDATA 'decurix/windows-runtime.json'
if (-not (Test-Path -LiteralPath $record)) { exit 0 }
$runtime = Get-Content -LiteralPath $record -Raw -Encoding UTF8 | ConvertFrom-Json
$distro = $runtime.distro
if ($distro -eq 'Decurix') { exit 0 }
if ($distro -isnot [string] -or [string]::IsNullOrWhiteSpace($distro) -or $distro -match '[\r\n\x00]' -or $distro -match '^docker-desktop') {
    throw 'Invalid saved WSL runtime; shared runtime cleanup was not run.'
}
$keep = @()
if ($Action -ne 'stop') {
    if ($null -eq $runtime.preexisting) {
        [Console]::Error.WriteLine("Nothing was removed from WSL runtime '$distro': this install began using it before Decurix recorded what was already there, so its own resources cannot be told from yours.")
        exit 3
    }
    $keep = @($runtime.preexisting | ForEach-Object { [string]$_ })
    foreach ($name in $keep) {
        if ($name -notmatch '^[A-Za-z0-9_.:/-]+$') { throw 'Invalid resource name in the saved WSL runtime; shared runtime cleanup was not run.' }
    }
}
$scriptPath = Join-Path $PSScriptRoot 'maintain-existing.sh'
$scriptPath = $scriptPath.Replace('\', '/')
if ($scriptPath -notmatch '^([A-Za-z]):/(.*)$') { throw 'Expected a Windows drive path for the maintenance script.' }
$linuxScript = '/mnt/' + $Matches[1].ToLowerInvariant() + '/' + $Matches[2]
# Sysnative lets the 32-bit NSIS uninstaller reach the real System32 wsl.exe.
$wslExe = Join-Path $env:WINDIR 'Sysnative/wsl.exe'
if (-not (Test-Path -LiteralPath $wslExe)) { $wslExe = Join-Path $env:WINDIR 'System32/wsl.exe' }
& $wslExe -d $distro -u root --cd / -e sh $linuxScript $Action @keep
if ($LASTEXITCODE -ne 0) { throw "Could not $Action Decurix resources in WSL runtime '$distro'. Start its Docker Engine and try again." }
