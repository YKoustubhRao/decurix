; What the stock installer does not do for Decurix: turn on what Windows needs to
; run its Linux, so the app opens after one restart (Windows 10 2004+ and 11).
;
; wsl\prepare-windows.ps1 turns on the WSL and Virtual Machine Platform
; features, installs WSL itself and lets the hypervisor start, behind one
; administrator prompt, only when something is missing. Its exit code is 100 plus bits:
; 1 restart, 2 virtualization off in the firmware, 4 failed, 8 prompt declined,
; 16 unsupported computer. A restart sets the reboot flag, so the finish page
; offers "Reboot now" in place of starting the app. What is left undone, the
; app does or explains at its first start (main.js linuxReady). An update skips
; all of this: the computer was made ready by the first install, and a silent
; update must not raise a prompt.
;
; 64-bit PowerShell (Sysnative), as in customUnInstall: a 32-bit one's dism.exe
; cannot change a 64-bit Windows.

!macro customInstall
  ${ifNot} ${isUpdated}
    Push $R2
    Push $R3
    Push $R4
    Push $R5
    StrCpy $R5 "$WINDIR\Sysnative\WindowsPowerShell\v1.0\powershell.exe"
    ${ifNot} ${FileExists} $R5
      StrCpy $R5 "$SYSDIR\WindowsPowerShell\v1.0\powershell.exe"
    ${endIf}
    DetailPrint "Checking that Windows can run Decurix's Linux (WSL, Virtual Machine Platform). Windows may ask you to allow changes; this can take a few minutes."
    nsExec::ExecToLog `"$R5" -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "$INSTDIR\resources\wsl\prepare-windows.ps1" -Action ensure`
    Pop $R4
    ; 100 plus the bits. Anything else ("error", "timeout", PowerShell's own 1
    ; for a script it would not run) is a failure, never a restart.
    ${if} $R4 < 100
    ${orIf} $R4 > 131
      StrCpy $R4 "4"
    ${else}
      IntOp $R4 $R4 - 100
    ${endIf}

    IntOp $R3 $R4 & 16
    ${if} $R3 != 0
      MessageBox MB_OK|MB_ICONEXCLAMATION "Decurix is installed, but this computer cannot run it: Decurix needs a 64-bit Intel or AMD computer with Windows 10 version 2004 or newer, or Windows 11. On Windows 10, install the latest updates from Windows Update, then install Decurix again." /SD IDOK
    ${else}
      IntOp $R3 $R4 & 8
      ${if} $R3 != 0
        MessageBox MB_OK|MB_ICONINFORMATION "Decurix is installed, but the Windows features it needs (Windows Subsystem for Linux, Virtual Machine Platform) were not turned on, because the administrator prompt was declined. Open Decurix and allow the prompt it shows, or install Decurix again." /SD IDOK
      ${endIf}
      IntOp $R3 $R4 & 4
      ${if} $R3 != 0
        MessageBox MB_OK|MB_ICONEXCLAMATION "Decurix is installed, but Windows Subsystem for Linux could not be fully set up (often: no internet connection, or a Windows 10 that lacks its latest updates). Restart Windows and open Decurix: it tries again. Details are in %LOCALAPPDATA%\Decurix\logs\prepare-windows.log." /SD IDOK
      ${endIf}
      IntOp $R2 $R4 & 1
      IntOp $R3 $R4 & 2
      ${if} $R3 != 0
        MessageBox MB_YESNO|MB_ICONEXCLAMATION|MB_DEFBUTTON2 "One more step, which only you can do: virtualization is switched off in this computer's firmware (BIOS/UEFI) settings, and Decurix's workers need it. Windows cannot turn it on.$\r$\n$\r$\nIn the firmware settings, turn on the option named Intel Virtualization Technology (VT-x), SVM Mode or AMD-V (often under Advanced, CPU Configuration or Security), save and exit. Then open Decurix.$\r$\n$\r$\nRestart into the firmware settings now? (Save your work first. If you choose No, restart and press the setup key, usually F2, F10, Del or Esc, while the computer starts.)" /SD IDNO IDNO firmwareLater
          nsExec::Exec `"$R5" -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "$INSTDIR\resources\wsl\prepare-windows.ps1" -Action firmware`
          Pop $R3
          ${if} $R3 == "0"
            ; Windows restarts into the firmware settings in 30 seconds.
            StrCpy $R2 "0"
          ${else}
            MessageBox MB_OK|MB_ICONINFORMATION "This computer cannot be restarted into its firmware settings from Windows. Restart it and press the setup key (usually F2, F10, Del or Esc) while it starts." /SD IDOK
          ${endIf}
        firmwareLater:
      ${endIf}
      ${if} $R2 != 0
        SetRebootFlag true
      ${endIf}
    ${endIf}
    Pop $R5
    Pop $R4
    Pop $R3
    Pop $R2
  ${endIf}
!macroend

; What the stock uninstaller does not know about Decurix.
;
; It ends every process running from the install folder (the window,
; cloudflared) and removes that folder. What lives elsewhere:
;
;   the WSL distro "Decurix"  Docker, the control plane, every worker desktop
;                             and its files, the records and worker images; its disk is %LOCALAPPDATA%\Decurix\wsl.
;                             Kept running by a wsl.exe the app started, which
;                             is not in the install folder - so it is stopped
;                             here, or the workers would go on running.
;   %APPDATA%\decurix         .env, the logs, the window's own profile
;
; The shared native model server lives in %LOCALAPPDATA%\Decurix\ollama
; outside WSL, and is ended here too. A person's own Ollama (another folder, port 11434) is not touched.
;
; Uninstalling deletes all of it, without asking, so the next install starts
; fresh: it used to ask with No as the default, and a person who clicked
; through found their old workers in the new install. --keep-app-data keeps it
; (a reinstall that should keep the workers). Never on an update, which runs
; this same uninstaller of the version being replaced.

!macro customUnInstall
  ${ifNot} ${isUpdated}
    ; The uninstaller is a 32-bit program, so $SYSDIR is SysWOW64, which has no
    ; wsl.exe; Sysnative is the real System32 seen from such a program.
    StrCpy $R6 "$WINDIR\Sysnative\wsl.exe"
    ${ifNot} ${FileExists} $R6
      StrCpy $R6 "$SYSDIR\wsl.exe"
    ${endIf}
    nsExec::Exec `"$SYSDIR\WindowsPowerShell\v1.0\powershell.exe" -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "$INSTDIR\resources\wsl\cleanup-existing.ps1" -Action stop`
    Pop $R8
    nsExec::Exec `"$R6" --terminate Decurix`
    Pop $R8
    nsExec::Exec `"$SYSDIR\WindowsPowerShell\v1.0\powershell.exe" -NoProfile -NonInteractive -C "$$store = Join-Path $$env:LOCALAPPDATA 'Decurix\ollama\'; Get-CimInstance -ClassName Win32_Process | ? { $$_.Path -and $$_.Path.StartsWith($$store, 'CurrentCultureIgnoreCase') } | % { Stop-Process -Id $$_.ProcessId -Force }"`
    Pop $R8

    StrCpy $R9 "1"
    ClearErrors
    ${GetParameters} $R7
    ${GetOptions} $R7 "--keep-app-data" $R8
    ${ifNot} ${Errors}
      StrCpy $R9 "0"
    ${endIf}
    ClearErrors

    ${if} $R9 == "1"
      ; A shared WSL distro is kept: delete only what Decurix created there
      ; (cleanup-existing.ps1 keeps what the runtime held before Decurix).
      nsExec::Exec `"$SYSDIR\WindowsPowerShell\v1.0\powershell.exe" -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "$INSTDIR\resources\wsl\cleanup-existing.ps1" -Action delete`
      Pop $R8
      ${if} $R8 == "3"
        ; An install from before Decurix kept a complete record of what that
        ; runtime already held: its own resources cannot be told from the
        ; person's, so none were removed.
        MessageBox MB_OK|MB_ICONINFORMATION "Nothing was removed from your own WSL runtime: Decurix has no complete record of what was already there when it began using it, so it cannot tell its resources from yours. If its desktop_* containers and volumes and the decurix-data volume are Decurix's, delete them there yourself." /SD IDOK
      ${elseIf} $R8 != "0"
        ; That runtime's Docker did not answer, or a removal failed: Decurix's
        ; workers there may stay, and the person is told. Everything Decurix
        ; owns outright still goes.
        MessageBox MB_OK|MB_ICONEXCLAMATION "Some of Decurix's resources in your own WSL runtime could not be removed (its Docker Engine did not answer, or a removal failed). Everything else was deleted. To remove those too, start that Docker Engine and delete Decurix's desktop_* containers and volumes there." /SD IDOK
      ${endIf}
      ; Unregistering the app-owned distro deletes its disk.
      ; Its forwarding rule (dockerd.sh fence) lives in the network stack WSL2 distros share, so it would outlive the
      ; distro: taken out first.
      nsExec::Exec `"$R6" -d Decurix -u root -e /opt/decurix/dockerd.sh unfence`
      nsExec::Exec `"$R6" --unregister Decurix`
      Pop $R8
      Sleep 1500
      ${if} $installMode == "all"
        SetShellVarContext current
      ${endif}
      RMDir /r "$LOCALAPPDATA\Decurix"
      RMDir /r "$APPDATA\decurix"
      ${if} $installMode == "all"
        SetShellVarContext all
      ${endif}
    ${endIf}
  ${endIf}
!macroend
