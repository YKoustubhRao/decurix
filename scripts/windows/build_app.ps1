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

    The app runs its Linux workers (Docker, XFCE, noVNC) in a WSL distro of
    its own, which it sets up itself on its first start - no Docker Desktop.

    It does not touch your data: workers, their files, chats and task history
    live in that distro (%LOCALAPPDATA%\Decurix\wsl), settings and logs in
    %APPDATA%\decurix, and the new build picks them up. Installing over a running app closes it first. -Fresh is the
    exception: it deletes all of it, so the new build starts as on a computer
    that never had Decurix.

    Run it from PowerShell (.\scripts\windows\build_app.ps1), or right-click
    the file and choose "Run with PowerShell". Needs Go, Node.js with npm, and
    an internet connection the first time.
.PARAMETER MinFreeMemoryMB
    Free RAM the build insists on before it starts (default 256; it builds
    slowly when little is free, with Go and Node capped).
.PARAMETER Run
    Start the freshly built app (the unpacked one) when the build is done.
.PARAMETER NoOpen
    Do not show the result in Explorer.
.PARAMETER Fresh
    Also delete everything Decurix has kept on this computer, for good: every
    worker and its files, chats, task history, taught skills, settings and
    keys, and paired devices. The next start is as on a computer that never
    had Decurix - except that the worker images and downloaded models are
    kept (they are a build and a download, not anyone's data), so it takes
    a minute, not the image build. Closes a running Decurix first, an
    installed one too.
#>
param(
    [int]$MinFreeMemoryMB = 256,
    [switch]$Run,
    [switch]$NoOpen,
    [switch]$Fresh
)

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$stage = Join-Path $repo 'desktop\electron\build'
$out = Join-Path $repo 'dist\electron'

# WMI can briefly retain an exited process while another process holds its
# handle. Such entries no longer lock the build and must not fail shutdown.
function Test-LiveProcess($ProcessId) {
    $process = Get-Process -Id $ProcessId -ErrorAction SilentlyContinue
    if (-not $process) { return $false }
    try { return -not $process.HasExited } finally { $process.Dispose() }
}

# The folder a process is working in. A program of Windows' own that the app
# started (curl.exe downloading Ollama, found holding win-unpacked ten minutes
# after the app was stopped) runs from System32, so its path says nothing - but
# it works in the app's folder, and Windows will not delete a folder that is
# somebody's working directory. Read from the process itself (64-bit layout);
# '' for one that cannot be read. A type stays loaded for the life of the
# PowerShell session, so a second run in the same window must not add it again.
if (-not ('DecurixWorkingDirectory' -as [type])) {
Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
using System.Text;
public static class DecurixWorkingDirectory {
    [DllImport("kernel32.dll")] static extern IntPtr OpenProcess(int access, bool inherit, int pid);
    [DllImport("kernel32.dll")] static extern bool CloseHandle(IntPtr handle);
    [DllImport("ntdll.dll")] static extern int NtQueryInformationProcess(IntPtr handle, int what, byte[] buffer, int size, out int returned);
    [DllImport("kernel32.dll")] static extern bool ReadProcessMemory(IntPtr handle, IntPtr address, byte[] buffer, IntPtr size, out IntPtr read);
    public static string Of(int pid) {
        if (IntPtr.Size != 8) return "";
        IntPtr handle = OpenProcess(0x0410, false, pid);
        if (handle == IntPtr.Zero) return "";
        try {
            byte[] basic = new byte[48]; int returned; IntPtr read;
            if (NtQueryInformationProcess(handle, 0, basic, basic.Length, out returned) != 0) return "";
            long peb = BitConverter.ToInt64(basic, 8);
            if (peb == 0) return "";
            byte[] pointer = new byte[8];
            if (!ReadProcessMemory(handle, (IntPtr)(peb + 0x20), pointer, (IntPtr)8, out read)) return "";
            byte[] text = new byte[16];
            if (!ReadProcessMemory(handle, (IntPtr)(BitConverter.ToInt64(pointer, 0) + 0x38), text, (IntPtr)16, out read)) return "";
            int length = BitConverter.ToUInt16(text, 0);
            byte[] path = new byte[length];
            if (length == 0 || !ReadProcessMemory(handle, (IntPtr)BitConverter.ToInt64(text, 8), path, (IntPtr)length, out read)) return "";
            return Encoding.Unicode.GetString(path);
        } catch { return ""; } finally { CloseHandle(handle); }
    }
}
'@
}

# Whether a process runs from, or works in, a folder.
function Test-ProcessIn($ProcessInfo, $Folder) {
    $prefix = $Folder.TrimEnd('\') + '\'
    if ($ProcessInfo.ExecutablePath -and $ProcessInfo.ExecutablePath.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)) { return $true }
    $cwd = [DecurixWorkingDirectory]::Of([int]$ProcessInfo.ProcessId)
    return [bool]$cwd -and ($cwd.TrimEnd('\') + '\').StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)
}

function Stop-DecurixProcess($ProcessInfo) {
    try {
        Stop-Process -Id $ProcessInfo.ProcessId -Force -ErrorAction Stop
    } catch {
        if (Test-LiveProcess $ProcessInfo.ProcessId) {
            return "$($ProcessInfo.Name) (PID $($ProcessInfo.ProcessId)): $($_.Exception.Message)"
        }
    }
}

# Every process of a running Decurix: the window and the control plane are
# both decurix.exe (the control plane in the app's resources folder), and the
# workers' agents and engines run from that same app folder. The control
# plane is detached and outlives the window, so both are looked for.
function Get-DecurixProcesses {
    $all = @(Get-CimInstance Win32_Process | Where-Object { $_.ExecutablePath -and (Test-LiveProcess $_.ProcessId) })
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
        $stopErrors = @($procs | ForEach-Object { Stop-DecurixProcess $_ })
        Start-Sleep -Seconds 1
    }
    $remaining = @(Get-DecurixProcesses)
    if ($remaining.Count -gt 0) {
        throw "Could not close Decurix; no data was deleted. $($stopErrors -join '; ') If access was denied, close Decurix from an elevated PowerShell or Task Manager, then retry."
    }

    # A shared WSL distro keeps other applications: reset only our resources.
    & powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File (Join-Path $repo 'desktop/electron/wsl/cleanup-existing.ps1') -Action reset
    if ($LASTEXITCODE -ne 0) { throw 'Could not reset the saved WSL runtime; its data and runtime record were kept.' }

    # The distro holds every worker and the records, and the worker images.
    # Fresh means no workers and no records - not building the images again,
    # which is ten minutes of downloads: reset.sh deletes every container and
    # every volume but the caches and the models, and keeps the images. The
    # distro is then stopped, so the app starts its daemon afresh. Should that
    # fail, the distro is unregistered: fresh all the same, only slower.
    $env:WSL_UTF8 = '1'
    $distros = @(& wsl.exe --list --quiet 2>$null | ForEach-Object { ($_ -replace "`0", '').Trim() })
    if ($distros -contains 'Decurix') {
        $reset = (Join-Path $repo 'desktop\electron\wsl\reset.sh') -replace '\\', '/'
        $reset = '/mnt/' + $reset.Substring(0, 1).ToLower() + $reset.Substring(2)
        Write-Host 'Deleting the workers and records in Decurix''s Linux (its built images are kept)'
        & wsl.exe -d Decurix -u root --cd / -e sh $reset
        if ($LASTEXITCODE -eq 0) {
            & wsl.exe --terminate Decurix 2>$null | Out-Null
        } else {
            Write-Host 'That did not work; deleting Decurix''s Linux whole instead'
            & wsl.exe --unregister Decurix 2>$null | Out-Null
        }
    }

    # An earlier version ran a model server on Windows itself, started detached
    # from the store. A person's own Ollama is elsewhere.
    # A download into the store (curl.exe fetching Ollama) is Windows' own
    # program with one of the store's files open: found by its command line.
    Get-CimInstance Win32_Process | Where-Object { $_.ProcessId -ne $PID -and ((Test-ProcessIn $_ $store) -or ($_.Name -ieq 'curl.exe' -and $_.CommandLine -and $_.CommandLine.IndexOf($store + '\', [StringComparison]::OrdinalIgnoreCase) -ge 0)) } |
        ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }

    # The store, but not the disk of a distro that was kept (its wsl folder).
    $targets = @($data)
    $distros = @(& wsl.exe --list --quiet 2>$null | ForEach-Object { ($_ -replace "`0", '').Trim() })
    if ($distros -contains 'Decurix') {
        $targets += @(Get-ChildItem $store -Force -ErrorAction SilentlyContinue | Where-Object { $_.Name -ne 'wsl' } | ForEach-Object { $_.FullName })
    } else {
        $targets += $store
    }
    foreach ($target in $targets) {
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
    @(Get-CimInstance Win32_Process | Where-Object { $_.ProcessId -ne $PID -and (Test-ProcessIn $_ $unpacked) -and (Test-LiveProcess $_.ProcessId) })
}
$running = Get-UnpackedProcesses
if ($running.Count -gt 0) {
    $window = @($running | Where-Object { $_.ExecutablePath -and (Split-Path -Parent $_.ExecutablePath).TrimEnd('\') -ieq $unpacked })
    if ($window.Count -gt 0 -and -not $Fresh) {
        throw "The app built last time is open (from $unpacked). Close its window, then build again."
    }

    Write-Host "Stopping the Decurix the last build left running in the background ($($running.Count) processes)"
    for ($try = 0; $running.Count -gt 0 -and $try -lt 10; $try++) {
        $stopErrors = @($running | ForEach-Object { Stop-DecurixProcess $_ })
        Start-Sleep -Seconds 1
        $running = Get-UnpackedProcesses
    }
    if ($running.Count -gt 0) {
        throw "Could not stop what is running from $unpacked. $($stopErrors -join '; ') If access was denied, close Decurix from an elevated PowerShell or Task Manager, then build again. No data was deleted."
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
    # reused. The pinned downloads (cloudflared, Alpine's rootfs) are kept -
    # each is checked against its SHA-256 before it is used, so a kept one is as
    # good as a fresh one and the build does not need them again.
    Write-Host ''
    Write-Host '=== Clearing the last build ==='
    # A program that was just stopped is gone from the process list a moment
    # before Windows lets go of its file: deleting at once failed with "Access
    # to the path 'Decurix.exe' is denied" right after the ten processes of the
    # last build were ended, and the file was free a few seconds later. So a
    # delete that fails is tried again for a while before it is an error.
    # An Explorer window showing the output - which the end of the last build
    # opened itself - holds the folder, and the delete below fails on it.
    try {
        foreach ($w in @((New-Object -ComObject Shell.Application).Windows())) {
            $where = try { $w.Document.Folder.Self.Path } catch { '' }
            if ($where -and $where.StartsWith($out, [StringComparison]::OrdinalIgnoreCase)) { $w.Quit() }
        }
    } catch { }

    $targets = @('runtime', 'stack', 'wsl', 'decurix.exe', 'decurix-android-arm64' | ForEach-Object { Join-Path $stage $_ }) + $out
    foreach ($path in $targets) {
        for ($try = 0; (Test-Path $path) -and $try -lt 15; $try++) {
            if ($try -gt 0) { Start-Sleep -Seconds 1 }
            Remove-Item $path -Recurse -Force -ErrorAction SilentlyContinue
        }
        if (Test-Path $path) {
            throw "Could not delete $path - something still has a file in it open. Close it and build again."
        }
    }

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
