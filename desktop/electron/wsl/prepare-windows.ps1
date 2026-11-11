# Run by the installer (installer.nsh, customInstall) so that Decurix opens after
# one restart: it turns on what Windows needs to run the app's Linux, on
# Windows 10 (2004 and newer) and Windows 11, Home included.
#
#   the Windows features   Microsoft-Windows-Subsystem-Linux and
#                          VirtualMachinePlatform (DISM; a restart finishes them)
#   WSL itself             the signed MSI from github.com/microsoft/WSL, never
#                          the Store (a Store download that fails says nothing;
#                          main.js ENABLE_WSL); `wsl --update --web-download` if
#                          GitHub cannot be reached that way
#   the hypervisor         `hypervisorlaunchtype off` in the boot settings is set
#                          back to auto
#   the firmware           virtualization switched off in the BIOS/UEFI cannot be
#                          turned on from Windows: it is found and reported, and
#                          -Action firmware restarts into those settings
#
# -Action ensure (the installer's call, as the person) looks first and asks for
# an administrator's yes only when something is missing, so an install on a
# ready computer shows no prompt. The answer is the exit code: 100 plus bits
#   1 restart needed   2 virtualization off in the firmware   4 something failed
#   8 the administrator prompt was declined   16 this computer cannot run it
# The 100 keeps PowerShell's own failures (exit 1: a script that a policy
# blocked or that did not parse) from reading as "restart needed"; the
# installer takes any code outside 100-131 as "failed".
# A restart that is needed is also written to %APPDATA%\decurix\
# restart-asked.json (reason "installer"), so the app says to restart rather
# than asking again before it (main.js, installerRestartPending).
#
# -Action decide prints the plan for the given facts (JSON) and changes nothing:
# the tests' entry (prepare-windows.test.js). -Action facts prints what this
# computer has, and changes nothing either.
param(
    [ValidateSet('ensure', 'enable', 'firmware', 'restart-to-firmware', 'decide', 'facts')][string]$Action = 'ensure',
    [string]$Log = '',
    [string]$Facts = ''
)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$RESTART = 1; $FIRMWARE = 2; $FAILED = 4; $DECLINED = 8; $UNSUPPORTED = 16
$BASE = 100
$MIN_BUILD = 19041

# The installer is a 32-bit program: its PowerShell would be the 32-bit one,
# whose dism.exe cannot service a 64-bit Windows and which has no wsl.exe.
if (-not $Log) { $Log = Join-Path $env:LOCALAPPDATA 'Decurix\logs\prepare-windows.log' }
if ($Action -ne 'decide' -and -not [Environment]::Is64BitProcess -and [Environment]::Is64BitOperatingSystem) {
    $native = Join-Path $env:WINDIR 'Sysnative\WindowsPowerShell\v1.0\powershell.exe'
    & $native -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $PSCommandPath -Action $Action -Log $Log
    exit $LASTEXITCODE
}

# Say prints a line for the installer's details and the log. Never
# Write-Output: in a function that becomes part of what it returns.
function Say([string]$text) {
    [Console]::Out.WriteLine($text)
    try {
        $dir = Split-Path -Parent $Log
        if (-not (Test-Path -LiteralPath $dir)) { New-Item -ItemType Directory -Force -Path $dir | Out-Null }
        Add-Content -LiteralPath $Log -Value ((Get-Date -Format 's') + " [$Action] " + $text) -Encoding UTF8
    } catch { }
}

# Run runs a program with no window and returns its exit code, or -1 when it
# could not start or ran past the bound (it is then ended).
function Run([string]$file, [string]$arguments, [int]$seconds) {
    try {
        $info = New-Object System.Diagnostics.ProcessStartInfo
        $info.FileName = $file
        $info.Arguments = $arguments
        $info.UseShellExecute = $false
        $info.CreateNoWindow = $true
        $info.RedirectStandardOutput = $true
        $info.RedirectStandardError = $true
        # Nothing it runs may wait on a key press.
        $info.RedirectStandardInput = $true
        $info.EnvironmentVariables['WSL_UTF8'] = '1'
        $process = [System.Diagnostics.Process]::Start($info)
        $process.StandardInput.Close()
        $out = $process.StandardOutput.ReadToEndAsync()
        $err = $process.StandardError.ReadToEndAsync()
        if (-not $process.WaitForExit($seconds * 1000)) {
            try { $process.Kill() } catch { }
            Say "$([IO.Path]::GetFileName($file)) $arguments did not finish in $seconds s."
            return -1
        }
        $process.WaitForExit()
        $said = (($out.Result + ' ' + $err.Result) -replace "`0", '' -replace '\s+', ' ').Trim()
        if ($said.Length -gt 300) { $said = $said.Substring(0, 300) }
        Say "$([IO.Path]::GetFileName($file)) $arguments -> $($process.ExitCode) $said"
        return $process.ExitCode
    } catch {
        Say "$file could not be run: $($_.Exception.Message)"
        return -1
    }
}

# WslAnswers: whether WSL itself is installed - the one that answers --version
# (the in-box one of an older Windows does not). Its own folder is asked too:
# the System32 one appears only once the feature is on, after a restart.
function WslAnswers {
    foreach ($exe in @((Join-Path $env:WINDIR 'System32\wsl.exe'), (Join-Path $env:ProgramFiles 'WSL\wsl.exe'))) {
        if ((Test-Path -LiteralPath $exe) -and (Run $exe '--version' 60) -eq 0) { return $true }
    }
    return $false
}

function FeatureOn([string]$name) {
    try {
        $feature = Get-CimInstance -ClassName Win32_OptionalFeature -Filter "Name='$name'"
        return ($null -ne $feature -and [int]$feature.InstallState -eq 1)
    } catch { return $false }
}

# GetFacts: what this computer has, all of it readable without an administrator.
function GetFacts {
    $arch = $env:PROCESSOR_ARCHITEW6432
    if (-not $arch) { $arch = $env:PROCESSOR_ARCHITECTURE }
    $build = 0
    try { $build = [int](Get-CimInstance -ClassName Win32_OperatingSystem).BuildNumber } catch { $build = [Environment]::OSVersion.Version.Build }
    $hypervisor = $false
    try { $hypervisor = [bool](Get-CimInstance -ClassName Win32_ComputerSystem).HypervisorPresent } catch { }
    # Both read false while a hypervisor runs (it hides them), so they matter
    # only when none does; $null is "not known", never "off".
    $firmware = $null; $extensions = $null
    try {
        $cpus = @(Get-CimInstance -ClassName Win32_Processor)
        if ($cpus.Count -gt 0) {
            $firmware = -not ($cpus | Where-Object { $_.VirtualizationFirmwareEnabled -eq $false })
            $extensions = -not ($cpus | Where-Object { $_.VMMonitorModeExtensions -eq $false })
        }
    } catch { }
    return [pscustomobject]@{
        arch          = [string]$arch
        build         = $build
        wslFeature    = FeatureOn 'Microsoft-Windows-Subsystem-Linux'
        vmpFeature    = FeatureOn 'VirtualMachinePlatform'
        wsl           = WslAnswers
        hypervisor    = $hypervisor
        firmware      = $firmware
        extensions    = $extensions
        rebootPending = Test-Path 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Component Based Servicing\RebootPending'
    }
}

# Decide: what ensure does for these facts - Elevate (run enable as an
# administrator) and Bits (what is known without doing so).
function Decide($f) {
    if ([string]$f.arch -match 'ARM') { return @{ Elevate = $false; Bits = $UNSUPPORTED; Why = 'arm' } }
    if ([int]$f.build -gt 0 -and [int]$f.build -lt $MIN_BUILD) { return @{ Elevate = $false; Bits = $UNSUPPORTED; Why = 'build' } }
    $bits = 0
    if (-not $f.hypervisor -and ($f.firmware -eq $false -or $f.extensions -eq $false)) { $bits = $bits -bor $FIRMWARE }
    $missing = (-not $f.wslFeature) -or (-not $f.vmpFeature) -or (-not $f.wsl)
    if ($missing) { return @{ Elevate = $true; Bits = $bits; Why = 'missing' } }
    if ($f.hypervisor) { return @{ Elevate = $false; Bits = $bits; Why = 'ready' } }
    # Everything is on but no hypervisor runs: a restart still owed, the
    # firmware, or a boot setting that keeps it off (which needs an administrator to read).
    if ($f.rebootPending) { return @{ Elevate = $false; Bits = ($bits -bor $RESTART); Why = 'restart' } }
    if ($bits -band $FIRMWARE) { return @{ Elevate = $false; Bits = $bits; Why = 'firmware' } }
    return @{ Elevate = $true; Bits = $bits; Why = 'hypervisor' }
}

# Elevated runs this script as an administrator (Windows asks the person) and
# returns its exit code, or -1 when the prompt was refused.
function Elevated([string]$action) {
    $arguments = "-NoProfile -NonInteractive -ExecutionPolicy Bypass -File `"$PSCommandPath`" -Action $action -Log `"$Log`""
    try {
        $process = Start-Process -FilePath (Join-Path $PSHOME 'powershell.exe') -ArgumentList $arguments -Verb RunAs -WindowStyle Hidden -Wait -PassThru
        return [int]$process.ExitCode
    } catch {
        Say "The administrator prompt was declined or failed: $($_.Exception.Message)"
        return -1
    }
}

# EnableFeature turns one Windows feature on; true when a restart now finishes it.
function EnableFeature([string]$name) {
    $state = ''
    try { $state = [string](Get-WindowsOptionalFeature -Online -FeatureName $name).State } catch { }
    if ($state -eq 'Enabled') { return $false }
    if ($state -eq 'EnablePending') { Say "$name is on once Windows restarts."; return $true }
    if (-not $state -and (FeatureOn $name)) { return $false }
    Say "Turning on $name..."
    $code = Run (Join-Path $env:WINDIR 'System32\dism.exe') "/online /enable-feature /featurename:$name /all /norestart" 900
    if ($code -eq 0 -or $code -eq 3010) { return $true }
    throw "Windows could not turn on $name (DISM exit $code)."
}

# InstallWslFromGitHub installs the latest WSL release's MSI, accepted only when
# Microsoft signed it (and when GitHub gives its digest, only with that digest).
function InstallWslFromGitHub {
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    $release = Invoke-RestMethod -UseBasicParsing -Uri 'https://api.github.com/repos/microsoft/WSL/releases/latest' -Headers @{ 'User-Agent' = 'Decurix-installer' } -TimeoutSec 60
    $asset = @($release.assets | Where-Object { $_.name -match '\.x64\.msi$' })[0]
    if (-not $asset) { throw "WSL release $($release.tag_name) has no x64 MSI." }
    $msi = Join-Path $env:TEMP ('decurix-' + $asset.name)
    Say "Downloading WSL $($release.tag_name)..."
    Invoke-WebRequest -UseBasicParsing -Uri $asset.browser_download_url -OutFile $msi -TimeoutSec 900
    try {
        if ($asset.digest -match '^sha256:([0-9a-fA-F]{64})$' -and (Get-FileHash -LiteralPath $msi -Algorithm SHA256).Hash -ne $Matches[1]) {
            throw 'The WSL download does not match the digest GitHub gives for it.'
        }
        $signature = Get-AuthenticodeSignature -LiteralPath $msi
        if ($signature.Status -ne 'Valid' -or $signature.SignerCertificate.Subject -notmatch 'O=Microsoft Corporation') {
            throw "The WSL download is not signed by Microsoft ($($signature.Status))."
        }
        $code = Run (Join-Path $env:WINDIR 'System32\msiexec.exe') "/i `"$msi`" /qn /norestart" 900
        if ($code -eq 3010) { return $true }
        if ($code -ne 0) { throw "The WSL installer failed (exit $code)." }
        return $false
    } finally {
        Remove-Item -LiteralPath $msi -Force -ErrorAction SilentlyContinue
    }
}

# SuspendBitLockerOnce pauses BitLocker on the system drive for the next restart
# only, as Windows does before its own boot changes: a changed boot setting can
# otherwise make that start ask for the recovery key. It resumes by itself.
function SuspendBitLockerOnce {
    $drive = $env:SystemDrive
    try {
        $volume = Get-BitLockerVolume -MountPoint $drive -ErrorAction Stop
        if ([string]$volume.ProtectionStatus -ne 'On') { return }
        Suspend-BitLocker -MountPoint $drive -RebootCount 1 -ErrorAction Stop | Out-Null
        Say "BitLocker on $drive is paused for the next restart only."
    } catch {
        # No BitLocker module (some Home editions): manage-bde does the same, and
        # fails harmlessly on a drive that is not encrypted.
        [void](Run (Join-Path $env:WINDIR 'System32\manage-bde.exe') "-protectors -disable $drive -RebootCount 1" 60)
    }
}

# Enable is the administrator's part: every step is tried, and what is left
# undone is in the exit code.
function Enable {
    $f = GetFacts
    $bits = (Decide $f).Bits -band $FIRMWARE
    foreach ($name in @('Microsoft-Windows-Subsystem-Linux', 'VirtualMachinePlatform')) {
        try { if (EnableFeature $name) { $bits = $bits -bor $RESTART } }
        catch { Say $_.Exception.Message; $bits = $bits -bor $FAILED }
    }
    if (-not (WslAnswers)) {
        $installed = $false
        try {
            if (InstallWslFromGitHub) { $bits = $bits -bor $RESTART }
            $installed = $true
        } catch {
            Say "WSL from GitHub: $($_.Exception.Message) Trying wsl --update --web-download."
            $exe = Join-Path $env:WINDIR 'System32\wsl.exe'
            if ((Test-Path -LiteralPath $exe) -and (Run $exe '--update --web-download' 900) -eq 0) { $installed = $true }
        }
        # Installed but not answering yet: on some Windows 10 it does once the
        # features are on, after the restart. Not a failure the person must act on.
        if (-not (WslAnswers)) {
            if ($installed) { Say 'WSL is installed and answers after the restart.'; $bits = $bits -bor $RESTART }
            else { Say 'WSL is still not installed.'; $bits = $bits -bor $FAILED }
        }
    }
    if (-not $f.hypervisor) {
        $bcdedit = Join-Path $env:WINDIR 'System32\bcdedit.exe'
        $boot = (& $bcdedit /enum '{current}' 2>&1 | Out-String)
        if ($boot -match '(?im)^\s*hypervisorlaunchtype\s+Off\s*$') {
            Say 'The boot settings keep the hypervisor off; setting hypervisorlaunchtype to auto.'
            SuspendBitLockerOnce
            if ((Run $bcdedit '/set {current} hypervisorlaunchtype auto' 60) -eq 0) { $bits = $bits -bor $RESTART }
            else { $bits = $bits -bor $FAILED }
        }
        if ($f.rebootPending) { $bits = $bits -bor $RESTART }
    }
    return $bits
}

# NoteRestart tells the app a restart is owed (main.js, installerRestartPending).
function NoteRestart {
    try {
        $dir = Join-Path $env:APPDATA 'decurix'
        if (-not (Test-Path -LiteralPath $dir)) { New-Item -ItemType Directory -Force -Path $dir | Out-Null }
        $at = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
        [IO.File]::WriteAllText((Join-Path $dir 'restart-asked.json'), "{`"reason`":`"installer`",`"at`":$at}`n")
    } catch { Say "Could not note the restart for the app: $($_.Exception.Message)" }
}

switch ($Action) {
    'decide' {
        # Windows PowerShell's ConvertFrom-Json hands an array on as one item: foreach unrolls it.
        $cases = ConvertFrom-Json -InputObject $Facts
        foreach ($case in $cases) {
            $plan = Decide $case
            Write-Output ("elevate=" + [int]$plan.Elevate + " bits=" + $plan.Bits + " why=" + $plan.Why)
        }
        exit 0
    }
    'facts' {
        GetFacts | ConvertTo-Json
        exit 0
    }
    'enable' {
        try { exit ($BASE + (Enable)) } catch { Say "Stopped: $($_.Exception.Message)"; exit ($BASE + $FAILED) }
    }
    'restart-to-firmware' {
        # /fw opens the UEFI settings on the next start; a computer with an old
        # BIOS has none, and says so with a nonzero exit.
        exit (Run (Join-Path $env:WINDIR 'System32\shutdown.exe') '/r /fw /t 30 /c "Restarting into the firmware settings to turn on virtualization for Decurix."' 60)
    }
    'firmware' {
        # shutdown's own code: 0 when the restart is set.
        exit (Elevated 'restart-to-firmware')
    }
    'ensure' {
        try {
            $f = GetFacts
            Say ("This computer: " + ($f | ConvertTo-Json -Compress))
            $plan = Decide $f
            $bits = $plan.Bits
            if ($plan.Elevate) {
                Say 'Turning on Windows Subsystem for Linux and the Virtual Machine Platform. Windows asks you to allow it.'
                $code = Elevated 'enable'
                if ($code -eq -1) { $bits = $bits -bor $DECLINED }
                elseif ($code -ge $BASE -and $code -le ($BASE + 31)) { $bits = $bits -bor ($code - $BASE) }
                else { Say "The administrator's part ended with exit $code."; $bits = $bits -bor $FAILED }
            }
            if ($bits -band $RESTART) { NoteRestart }
            Say "Done ($($plan.Why)): bits $bits."
            exit ($BASE + $bits)
        } catch {
            Say "Stopped: $($_.Exception.Message)"
            exit ($BASE + $FAILED)
        }
    }
}
