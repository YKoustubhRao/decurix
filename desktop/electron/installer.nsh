; What the stock uninstaller does not know about Decurix.
;
; It ends every process running from the install folder (the window,
; cloudflared) and removes that folder. What lives elsewhere:
;
;   the WSL distro "Decurix"  Docker, the control plane, every worker desktop
;                             and its files, the records, the worker images and
;                             the models; its disk is %LOCALAPPDATA%\Decurix\wsl.
;                             Kept running by a wsl.exe the app started, which
;                             is not in the install folder - so it is stopped
;                             here, or the workers would go on running.
;   %APPDATA%\decurix         .env, the logs, the window's own profile
;
; An earlier version ran its control plane and a model server on Windows
; itself; the model server lived in %LOCALAPPDATA%\Decurix\ollama and is ended
; too. A person's own Ollama (another folder, port 11434) is not touched.
;
; The data is the person's: kept unless they say otherwise. Asked once, default
; No; a silent uninstall keeps it unless run with --delete-app-data. Never on
; an update, which runs this same uninstaller of the version being replaced.

!macro customUnInstall
  ${ifNot} ${isUpdated}
    ; The uninstaller is a 32-bit program, so $SYSDIR is SysWOW64, which has no
    ; wsl.exe; Sysnative is the real System32 seen from such a program.
    StrCpy $R6 "$WINDIR\Sysnative\wsl.exe"
    ${ifNot} ${FileExists} $R6
      StrCpy $R6 "$SYSDIR\wsl.exe"
    ${endIf}
    nsExec::Exec `"$SYSDIR\WindowsPowerShell\v1.0\powershell.exe" -NoProfile -NonInteractive -File "$INSTDIR\resources\wsl\cleanup-existing.ps1" -Action stop`
    Pop $R8
    nsExec::Exec `"$R6" --terminate Decurix`
    Pop $R8
    nsExec::Exec `"$SYSDIR\WindowsPowerShell\v1.0\powershell.exe" -NoProfile -NonInteractive -C "$$store = Join-Path $$env:LOCALAPPDATA 'Decurix\ollama\'; Get-CimInstance -ClassName Win32_Process | ? { $$_.Path -and $$_.Path.StartsWith($$store, 'CurrentCultureIgnoreCase') } | % { Stop-Process -Id $$_.ProcessId -Force }"`
    Pop $R8

    StrCpy $R9 "0"
    ClearErrors
    ${GetParameters} $R7
    ${GetOptions} $R7 "--delete-app-data" $R8
    ${ifNot} ${Errors}
      StrCpy $R9 "1"
    ${else}
      MessageBox MB_YESNO|MB_ICONQUESTION|MB_DEFBUTTON2 "Also delete your workers and their files, task history, the downloaded worker desktop and model, and your settings?$\r$\n$\r$\nChoose No to keep them for the next time Decurix is installed." /SD IDNO IDNO decurixKeepData
      StrCpy $R9 "1"
      decurixKeepData:
    ${endIf}
    ClearErrors

    ${if} $R9 == "1"
      ; A shared WSL distro is kept: delete only Decurix-owned resources.
      nsExec::Exec `"$SYSDIR\WindowsPowerShell\v1.0\powershell.exe" -NoProfile -NonInteractive -File "$INSTDIR\resources\wsl\cleanup-existing.ps1" -Action delete`
      Pop $R8
      ${if} $R8 != "0"
        MessageBox MB_OK|MB_ICONEXCLAMATION "The shared WSL runtime could not be cleaned up. Its workers and saved runtime settings were kept. Start its Docker Engine before trying to delete the data again." /SD IDOK
      ${else}
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
  ${endIf}
!macroend
