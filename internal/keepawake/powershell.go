package keepawake

import (
	"encoding/base64"
	"unicode/utf16"
)

// powerShellHelper is the Windows half of pipeHolder: it holds
// ES_CONTINUOUS|ES_SYSTEM_REQUIRED on its one thread while told "on", clears
// it on "off", and exits when its stdin closes - which also drops the
// request, since Windows keeps it only for a thread that exists. The numbers
// are written in decimal because Windows PowerShell reads 0x80000001 as a
// negative int32 that no longer fits a uint32.
const powerShellHelper = `$ErrorActionPreference = 'Stop'
Add-Type -Namespace Decurix -Name Power -MemberDefinition '[DllImport("kernel32.dll")] public static extern uint SetThreadExecutionState(uint esFlags);'
while ($true) {
  $line = [Console]::In.ReadLine()
  if ($null -eq $line) { break }
  if ($line -eq 'on') { [void][Decurix.Power]::SetThreadExecutionState([uint32]2147483649) }
  elseif ($line -eq 'off') { [void][Decurix.Power]::SetThreadExecutionState([uint32]2147483648) }
}
`

// encodedCommand is script as -EncodedCommand wants it (base64 of UTF-16LE),
// so no quote in it has to survive being passed as a command-line argument.
func encodedCommand(script string) string {
	units := utf16.Encode([]rune(script))
	raw := make([]byte, 2*len(units))

	for i, u := range units {
		raw[2*i] = byte(u)
		raw[2*i+1] = byte(u >> 8)
	}

	return base64.StdEncoding.EncodeToString(raw)
}
