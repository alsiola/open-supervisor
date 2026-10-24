//go:build windows

package worktree

// killWithin has nothing to find processes by on Windows (no pgrep or lsof), so it leaves them be.
func killWithin(string) {}
