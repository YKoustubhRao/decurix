<#
.SYNOPSIS
    Shared by scripts/windows/build.ps1 and scripts/windows/rebuild.ps1.
.DESCRIPTION
    Dot-sourced, not executed directly: defines Start-KeepAwake/
    Stop-KeepAwake.

    The control plane runs in a container (in Docker Desktop's VM), which
    cannot stop Windows from going to sleep - and nothing runs on a sleeping
    machine: no email trigger, no schedule, no desktop. So while this script
    runs, a hidden helper asks the control plane (GET /api/keep-awake)
    whether anything needs the machine awake (a watched mailbox, a waiting
    schedule) and holds a SetThreadExecutionState request only while it does
    - see internal/keepawake, which does the same itself when the binary
    runs directly on a host.

    It stops *idle* sleep only. Closing the lid or choosing Sleep still
    sleeps the machine; email that arrived meanwhile is picked up on waking.
    Off with DECURIX_KEEP_AWAKE=0 in .env.

    Requires: Read-EnvVar (lib/Ollama-Setup.ps1), dot-sourced by the caller
    first.
#>

# The helper holds ES_CONTINUOUS|ES_SYSTEM_REQUIRED on its one thread while
# the control plane wants it, clears it when not, and exits - which drops the
# request - once the script that started it (PARENT_PID) is gone, however it
# ended. The numbers are decimal because Windows PowerShell reads 0x80000001
# as a negative int32 that does not fit a uint32.
$script:KeepAwakeHelper = @'
$ErrorActionPreference = 'SilentlyContinue'
Add-Type -Namespace Decurix -Name Power -MemberDefinition '[DllImport("kernel32.dll")] public static extern uint SetThreadExecutionState(uint esFlags);'
while (Get-Process -Id PARENT_PID -ErrorAction SilentlyContinue) {
  $wanted = $false
  try { $wanted = [bool](Invoke-RestMethod -Uri 'STATE_URL' -TimeoutSec 5).wanted } catch {}
  if ($wanted) { [void][Decurix.Power]::SetThreadExecutionState([uint32]2147483649) }
  else { [void][Decurix.Power]::SetThreadExecutionState([uint32]2147483648) }
  Start-Sleep -Seconds 30
}
'@

# Start-KeepAwake starts the hidden helper. Sets $script:KeepAwakeProcess for
# Stop-KeepAwake.
function Start-KeepAwake {
    param([string]$StateUrl = "http://127.0.0.1:8080/api/keep-awake?holder=script")

    $setting = "$(Read-EnvVar 'DECURIX_KEEP_AWAKE')".Trim().ToLowerInvariant()
    if (@('0', 'false', 'off', 'no') -contains $setting) {
        Write-Host "Keep-awake is off (DECURIX_KEEP_AWAKE=0): this machine may sleep, and nothing runs while it does."
        return
    }

    $script = $script:KeepAwakeHelper.Replace('PARENT_PID', "$PID").Replace('STATE_URL', $StateUrl)
    $encoded = [Convert]::ToBase64String([System.Text.Encoding]::Unicode.GetBytes($script))
    $powershell = (Get-Process -Id $PID).Path

    $script:KeepAwakeProcess = Start-Process -FilePath $powershell `
        -ArgumentList @('-NoProfile', '-NonInteractive', '-WindowStyle', 'Hidden', '-EncodedCommand', $encoded) `
        -WindowStyle Hidden -PassThru

    Write-Host "Keep-awake: while mailboxes or schedules are active, this machine won't go to sleep on its own"
    Write-Host "(for as long as this script runs; closing the lid still sleeps it). DECURIX_KEEP_AWAKE=0 in .env turns this off."
}

# Stop-KeepAwake ends the helper, which releases its request.
function Stop-KeepAwake {
    if ($script:KeepAwakeProcess -and -not $script:KeepAwakeProcess.HasExited) {
        Stop-Process -Id $script:KeepAwakeProcess.Id -Force -ErrorAction SilentlyContinue
    }

    $script:KeepAwakeProcess = $null
}
