//go:build !windows

package worktree

import (
	"os"
	"syscall"
)

// lockFile waits for an exclusive lock on f, which closing f lets go.
func lockFile(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			return err
		}
	}
}
