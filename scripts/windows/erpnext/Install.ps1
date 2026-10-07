param(
    [string]$StackPath = (Join-Path $env:USERPROFILE 'erpnext-demo'),
    [string]$InstallDir = (Join-Path $env:ProgramFiles 'ERPNext')
)
$ErrorActionPreference = 'Stop'
if (-not (Test-Path -LiteralPath (Join-Path $StackPath 'pwd.yml'))) { throw 'The existing ERPNext stack needs pwd.yml.' }
$compiler = Join-Path $env:WINDIR 'Microsoft.NET\Framework64\v4.0.30319\csc.exe'
$source = Join-Path $PSScriptRoot 'Program.cs'
$output = Join-Path $env:TEMP ('ERPNext-' + [guid]::NewGuid().ToString('N') + '.exe')
$icon = Join-Path $InstallDir 'ERPNext.ico'
& $compiler /nologo /target:winexe /reference:System.Windows.Forms.dll "/win32icon:$icon" "/out:$output" $source
if ($LASTEXITCODE -ne 0) { throw 'ERPNext launcher compilation failed.' }
$exe = Join-Path $InstallDir 'ERPNext.exe'
if (Test-Path -LiteralPath $exe) { Copy-Item -LiteralPath $exe -Destination (Join-Path $InstallDir 'ERPNext.previous.exe') -Force }
Copy-Item -LiteralPath $output -Destination $exe -Force
Set-Content -LiteralPath (Join-Path $InstallDir 'stack-path.txt') -Value (Resolve-Path -LiteralPath $StackPath).Path
$shortcut = (New-Object -ComObject WScript.Shell).CreateShortcut((Join-Path ([Environment]::GetFolderPath('Desktop')) 'ERPNext (India Compliance).lnk'))
$shortcut.TargetPath = $exe
$shortcut.Arguments = ''
$shortcut.WorkingDirectory = $InstallDir
$shortcut.IconLocation = "$icon,0"
$shortcut.Save()
Remove-Item -LiteralPath $output
Write-Host 'ERPNext is installed. Open the desktop shortcut to start it.'
