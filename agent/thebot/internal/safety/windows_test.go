package safety

import "testing"

// The rules for a person's own Windows computer, checked on any system: they
// are text patterns, and a worker's PowerShell command is text.
func TestWindowsCommandsThatNeedApproval(t *testing.T) {
	stopped := map[string]string{
		// Deleting what is the person's, not the worker's.
		`Remove-Item -Recurse -Force C:\Users\me\Documents`: "outside this worker's own folder",
		`Remove-Item "C:\Users\me\Desktop\report.docx"`:     "outside this worker's own folder",
		`rm -r ~\Downloads`:                                  "outside this worker's own folder",
		`del /s /q D:\photos\*.jpg`:                          "outside this worker's own folder",
		`Remove-Item $env:USERPROFILE\notes.txt`:             "outside this worker's own folder",
		`Remove-Item ..\..\..\other-worker -Recurse`:         "outside this worker's own folder",
		`cd C:\; Remove-Item \\fileserver\share\budget.xlsx`: "outside this worker's own folder",
		// What is deleted comes from another folder: down a pipeline, or
		// because the command went there first.
		`Get-ChildItem C:\Temp | ForEach-Object { Remove-Item $_.FullName }`: "outside this worker's own folder",
		`cd C:\Users\me\Documents; Remove-Item notes.txt`:                    "outside this worker's own folder",
		`Format-Volume -DriveLetter D`:                                       "erase a disk",
		`format D: /q`:                                                       "erase a disk",
		`Stop-Computer -Force`:                                               "shuts down",
		`shutdown /r /t 0`:                                                   "shuts down",
		`Stop-Process -Name chrome`:                                          "ends running programs",
		`taskkill /IM excel.exe /F`:                                          "ends running programs",
		`Set-ItemProperty -Path HKCU:\Software\Foo -Name Bar -Value 1`:       "registry",
		`reg delete HKLM\Software\Foo /f`:                                    "registry",
		`winget uninstall Mozilla.Firefox`:                                   "uninstalls software",
		`Set-ExecutionPolicy Unrestricted`:                                   "security or system settings",
		`Set-MpPreference -DisableRealtimeMonitoring $true`:                  "security or system settings",
		`net user guest /active:yes`:                                         "user accounts",
		`Start-Process powershell -Verb RunAs`:                               "administrator rights",
		`Send-MailMessage -To boss@example.com -Subject hi`:                  "sends email",
		`Invoke-RestMethod -Uri https://api.example.com/pay -Method Post`:    "sends data to a web service",
		`iwr https://example.com/upload -Body $data`:                         "sends data to a web service",
	}

	for command, want := range stopped {
		got := assessWindows(command)
		switch {
		case want == "" && got != nil:
			t.Errorf("%q was stopped (%s)", command, got.Reason)
		case want != "" && got == nil:
			t.Errorf("%q ran without approval; want a stop for: %s", command, want)
		case want != "" && !contains(got.Reason, want):
			t.Errorf("%q stopped because %q, want a reason about %q", command, got.Reason, want)
		}
	}
}

// Ordinary work, and anything inside the worker's own folder, is not asked
// about: a gate that fires on it teaches a person to approve without reading.
func TestWindowsCommandsThatRunFreely(t *testing.T) {
	free := []string{
		`$env:COMPUTERNAME`,
		`Get-ChildItem C:\Windows\System32\drivers\etc`,
		`(Get-ChildItem C:\Users\me\Documents).Count`,
		`Get-Content C:\Users\me\Documents\notes.txt`,
		`Set-Content -Path notes.txt -Value "hello"`,
		`Copy-Item C:\Users\me\Documents\report.docx .`,
		`Remove-Item build -Recurse -Force`,
		`Remove-Item .\out\old.txt`,
		`del notes.bak`,
		`Remove-Item "C:\Users\me\AppData\Local\Decurix\workers\ada\home\work\tmp" -Recurse`,
		`Get-Process | Sort-Object CPU -Descending | Select-Object -First 5`,
		`Start-Process charmap.exe`,
		`Invoke-WebRequest -UseBasicParsing https://example.com`,
		`Invoke-RestMethod -Uri http://127.0.0.1:3000/items -Method Post -Body '{}'`,
		`Get-ItemProperty 'C:\Program Files\App\app.exe'`,
		`python -c "print(format(3.14159, '.2f'))"`,
		`Write-Output "remove-item is a cmdlet"`,
	}

	for _, command := range free {
		if got := assessWindows(command); got != nil {
			t.Errorf("%q was stopped: %s", command, got.Reason)
		}
	}
}

func contains(s, part string) bool {
	for i := 0; i+len(part) <= len(s); i++ {
		if s[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
