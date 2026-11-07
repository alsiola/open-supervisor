//go:build windows

package worktree

// killWithin has nothing to find processes by on Windows (no pgrep or lsof), so it leaves them be.
func killWithin(string) {}

// KillOrphans has nothing to find processes by on Windows either.
func KillOrphans(string) int { return 0 }
