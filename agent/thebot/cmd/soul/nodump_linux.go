// nodump_linux.go: soul makes itself non-dumpable at start, so the worker's commands (the same Linux user) cannot read
// its token from its /proc files or memory.
package main

import "syscall"

// prSetDumpable is prctl's PR_SET_DUMPABLE.
const prSetDumpable = 4

// noDump makes this process non-dumpable (prctl PR_SET_DUMPABLE 0): its /proc files and memory become root's, and no
// other process of this user may ptrace it. A program soul runs is dumpable again (exec resets it).
func noDump() error {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0); errno != 0 {
		return errno
	}
	return nil
}
