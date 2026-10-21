package httpapi

import (
	"encoding/base64"
	"strings"
	"testing"
	"unicode/utf16"
)

// TestReapScriptEndsOnlyWhatItStarted: the helper's tree only while it is still a helper; the app's only while it is
// still that helper's child - a process id the host gave to the person's own program is left alone.
func TestReapScriptEndsOnlyWhatItStarted(t *testing.T) {
	s := reapScript(4242, 42)
	for _, want := range []string{
		"ProcessId=42'", "$a.ParentProcessId -eq 4242", "taskkill.exe /T /F /PID 42 ",
		"ProcessId=4242'", "$h.CommandLine -like '*decurix-winhost*bridge.ps1*'", "taskkill.exe /T /F /PID 4242 ",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("reap script lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(reapScript(4242, 0), "ProcessId=0") || strings.Contains(reapScript(0, 42), "ProcessId=0") {
		t.Error("an unknown id (0) must never be looked up or ended")
	}
	if strings.Count(s, "taskkill") != 2 {
		t.Errorf("want exactly the two guarded kills:\n%s", s)
	}
}

// TestSweepScriptEndsOnlyHelpersNotKept: every PowerShell running the helper script, except the ones kept.
func TestSweepScriptEndsOnlyHelpersNotKept(t *testing.T) {
	s := sweepScript([]int{11, 22})
	for _, want := range []string{"Name='powershell.exe'", "-like '*decurix-winhost*bridge.ps1*'", "$keep = @(11,22)",
		"$keep -notcontains [int]$_.ProcessId", "taskkill.exe /T /F /PID $_.ProcessId"} {
		if !strings.Contains(s, want) {
			t.Errorf("sweep script lacks %q:\n%s", want, s)
		}
	}
	if !strings.Contains(sweepScript(nil), "$keep = @()") {
		t.Error("at start nothing is kept")
	}
}

func TestEncodedCommandIsUTF16Base64(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString(encodedCommand("Get-Process 'é'"))
	if err != nil || len(raw)%2 != 0 {
		t.Fatalf("not base64 UTF-16: %v", err)
	}
	u := make([]uint16, len(raw)/2)
	for i := range u {
		u[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	if got := string(utf16.Decode(u)); got != "Get-Process 'é'" {
		t.Fatalf("round trip = %q", got)
	}
}

// TestSweepLogReadsOnlyItsOwnLines: PowerShell's progress records (CLIXML) were logged as if they were ended helpers.
func TestSweepLogReadsOnlyItsOwnLines(t *testing.T) {
	out := "#< CLIXML\n<Objs Version=\"1.1.0.1\"><Obj S=\"progress\">Preparing modules for first use.</Obj></Objs>\n" +
		"ENDED 24384\nENDED 14736\nENDED x\n"
	if got := strings.Join(endedPIDs(out), ","); got != "24384,14736" {
		t.Fatalf("endedPIDs = %q", got)
	}
	for _, s := range []string{reapScript(1, 2), sweepScript(nil)} {
		if !strings.Contains(s, "$ProgressPreference = 'SilentlyContinue'") {
			t.Fatalf("no progress records wanted:\n%s", s)
		}
	}
}

// TestReleaseEndsOnlyOurHolders: the file's holders by Restart Manager; one is ended only when a parent is our helper.
func TestReleaseEndsOnlyOurHolders(t *testing.T) {
	s := releaseScript(`C:\Users\me\Documents\Decurix demo\process\P1\Exceptions_memo_Oct2026.docx`)
	for _, want := range []string{"rstrtmgr.dll", "RmGetList", `[DecurixRm]::Holders('C:\Users\me\Documents`,
		"$cur.CommandLine -like '*decurix-winhost*bridge.ps1*'", "if ($ours) { taskkill.exe /T /F /PID $id",
		"'HELD ' + $id", "$ProgressPreference = 'SilentlyContinue'"} {
		if !strings.Contains(s, want) {
			t.Errorf("release script lacks %q", want)
		}
	}
	if q := releaseScript(`C:\a\it's.docx`); !strings.Contains(q, `Holders('C:\a\it''s.docx')`) {
		t.Errorf("a quote in the path must be doubled:\n%s", q)
	}
	released, held := releaseLines("noise\nRELEASED 26456 WINWORD.EXE\nHELD 4242 WINWORD.EXE\n")
	if strings.Join(released, ",") != "WINWORD.EXE 26456" || strings.Join(held, ",") != "WINWORD.EXE 4242" {
		t.Fatalf("released %v held %v", released, held)
	}
}
