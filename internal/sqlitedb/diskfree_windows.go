//go:build windows

package sqlitedb

import "golang.org/x/sys/windows"

// freeDisk is the bytes free to this user on the volume holding dir.
func freeDisk(dir string) (uint64, error) {
	path, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(path, &free, &total, &totalFree); err != nil {
		return 0, err
	}
	return free, nil
}
