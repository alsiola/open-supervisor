package host

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func diskSpace(path string) (total, free uint64, id string, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, "", err
	}
	err = windows.GetDiskFreeSpaceEx(p, &free, &total, nil)
	if abs, e := filepath.Abs(path); e == nil {
		path = abs
	}
	return total, free, strings.ToUpper(filepath.VolumeName(path)), err
}
