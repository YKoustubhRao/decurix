package skills

// Tests name documents that are not on this machine: every named file is taken to be there unless a test says so.
func init() {
	sourceMissing = func(string) bool { return false }
}
