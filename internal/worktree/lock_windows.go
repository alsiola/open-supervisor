//go:build windows

package worktree

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFile waits for an exclusive lock on f, which closing f lets go.
func lockFile(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &windows.Overlapped{})
}
