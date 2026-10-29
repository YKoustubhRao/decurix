package qualification

// SourceFingerprint is the hash SaveDraft pins for a source, so a caller can
// tell whether the worker's skill or recording changed since the suite was drafted.
func SourceFingerprint(src PracticeSource) string {
	return sourceFingerprint(src)
}
