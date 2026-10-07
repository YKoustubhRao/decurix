param([string]$Request)
$ErrorActionPreference = 'Stop'
$r = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($Request)) | ConvertFrom-Json
Add-Type -AssemblyName System.Drawing
Add-Type -ReferencedAssemblies System.Drawing @'
using System;
using System.Drawing;
using System.Runtime.InteropServices;
public static class AppIcon {
    [DllImport("shell32.dll", CharSet=CharSet.Unicode)] static extern uint ExtractIconEx(string file, int index, IntPtr[] large, IntPtr[] small, uint count);
    [DllImport("user32.dll")] static extern bool DestroyIcon(IntPtr icon);
    public static Bitmap Read(string path, int index) {
        var large = new IntPtr[1]; var small = new IntPtr[1];
        try {
            if (ExtractIconEx(path, index, large, small, 1) == 0) return null;
            var h = large[0] != IntPtr.Zero ? large[0] : small[0];
            if (h == IntPtr.Zero) return null;
            using (var icon = Icon.FromHandle(h)) return icon.ToBitmap();
        } finally {
            if (large[0] != IntPtr.Zero) DestroyIcon(large[0]);
            if (small[0] != IntPtr.Zero) DestroyIcon(small[0]);
        }
    }
}
'@
$target = [Environment]::ExpandEnvironmentVariables([string]$r.target)
if ($target -like 'shell:AppsFolder*') {
    $roots = @("$env:APPDATA\Microsoft\Windows\Start Menu\Programs", "$env:ProgramData\Microsoft\Windows\Start Menu\Programs", [Environment]::GetFolderPath('Desktop'))
    $shortcut = Get-ChildItem -LiteralPath $roots -Filter '*.lnk' -Recurse -ErrorAction SilentlyContinue |
        Where-Object { $_.BaseName -eq $r.name } | Select-Object -First 1
    if (-not $shortcut) { exit 0 }
    $target = $shortcut.FullName
}
$index = 0
if ($target -like '*.lnk') {
    $shortcut = (New-Object -ComObject WScript.Shell).CreateShortcut($target)
    $location = [string]$shortcut.IconLocation
    if ($location -match '^(.*),\s*(-?\d+)$' -and $Matches[1]) {
        $target = $Matches[1].Trim('"'); $index = [int]$Matches[2]
    } else { $target = $shortcut.TargetPath }
}
$target = [Environment]::ExpandEnvironmentVariables($target)
if (-not (Test-Path -LiteralPath $target)) {
    $program = Get-Command $target -ErrorAction SilentlyContinue
    if (-not $program) { exit 0 }
    $target = $program.Source
}
$bitmap = [AppIcon]::Read($target, $index)
if (-not $bitmap) { exit 0 }
$stream = New-Object IO.MemoryStream
try {
    $bitmap.Save($stream, [Drawing.Imaging.ImageFormat]::Png)
    [Console]::Write([Convert]::ToBase64String($stream.ToArray()))
} finally { $bitmap.Dispose(); $stream.Dispose() }
