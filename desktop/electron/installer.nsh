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
