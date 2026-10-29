package ollama

// AvailableMemory reports this machine's total and available memory in bytes, as
// the OS counts it (free plus reclaimable cache), without adding back what a
// loaded model holds. A practice run refuses to start when the available figure
// is short (internal/practice).
func AvailableMemory() (total, avail uint64, err error) {
	return readMemory()
}
