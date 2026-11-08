package runs

import (
	"fmt"
	"strconv"
	"strings"
)

// BundleLimitEnv names the setting for the per-run bundle limit, in MB (docs/running.md).
const BundleLimitEnv = "DECURIX_RUN_BUNDLE_MB"

// The bounds of BundleLimitEnv, in MB: below 8 a run keeps hardly a frame, above 1024 one run could fill a small disk.
const (
	MinBundleMB = 8
	MaxBundleMB = 1024
)

// BundleLimit is the per-run bundle limit in bytes that raw (BundleLimitEnv's value) names: DefaultLimit when it is
// empty, the nearest bound when it is outside MinBundleMB..MaxBundleMB, DefaultLimit when it is no whole number. note
// says what was done with a value that was not taken as written ("" when it was, or was empty), for the start log.
func BundleLimit(raw string) (limit int64, note string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultLimit, ""
	}
	mb, err := strconv.ParseInt(raw, 10, 64)
	switch {
	case err != nil:
		return DefaultLimit, fmt.Sprintf("%s=%q is not a whole number of MB; using %d MB", BundleLimitEnv, raw, DefaultLimit>>20)
	case mb < MinBundleMB:
		return MinBundleMB << 20, fmt.Sprintf("%s=%d is below %d; using %d MB", BundleLimitEnv, mb, MinBundleMB, MinBundleMB)
	case mb > MaxBundleMB:
		return MaxBundleMB << 20, fmt.Sprintf("%s=%d is above %d; using %d MB", BundleLimitEnv, mb, MaxBundleMB, MaxBundleMB)
	}
	return mb << 20, ""
}
