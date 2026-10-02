; What the stock uninstaller does not know about Decurix.
;
; It ends every process running from the install folder (the window, the
; control plane, the workers' agents and engines, cloudflared) and removes that
; folder. Two things live elsewhere:
;
;   %LOCALAPPDATA%\Decurix   the store: workers and their files, the local
;                            model server the control plane fetched for itself,
;                            and the models it pulled (gigabytes)
;   %APPDATA%\decurix        the database, the keys, .env, the logs
;
; The model server is started detached from the store and deliberately left
; running between runs of the app. Uninstalling left it serving on 11435 with a
; model in memory, and nothing on the computer to stop it with - so it is ended
; here. A person's own Ollama (another folder, port 11434) is not touched.
;
; The data is the person's: kept unless they say otherwise. Asked once, default
; No; a silent uninstall keeps it unless run with --delete-app-data. Never on
; an update, which runs this same uninstaller of the version being replaced.

!macro customUnInstall
  ${ifNot} ${isUpdated}
    nsExec::Exec `"$SYSDIR\WindowsPowerShell\v1.0\powershell.exe" -NoProfile -NonInteractive -C "$$store = Join-Path $$env:LOCALAPPDATA 'Decurix\ollama\'; Get-CimInstance -ClassName Win32_Process | ? { $$_.Path -and $$_.Path.StartsWith($$store, 'CurrentCultureIgnoreCase') } | % { Stop-Process -Id $$_.ProcessId -Force }"`
    Pop $R8

    StrCpy $R9 "0"
    ClearErrors
    ${GetParameters} $R7
    ${GetOptions} $R7 "--delete-app-data" $R8
    ${ifNot} ${Errors}
      StrCpy $R9 "1"
    ${else}
      MessageBox MB_YESNO|MB_ICONQUESTION|MB_DEFBUTTON2 "Also delete your workers and their files, the downloaded model, and your settings?$\r$\n$\r$\nChoose No to keep them for the next time Decurix is installed." /SD IDNO IDNO decurixKeepData
      StrCpy $R9 "1"
      decurixKeepData:
    ${endIf}
    ClearErrors

    ${if} $R9 == "1"
      ; The model server needs a moment to let go of its files.
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
