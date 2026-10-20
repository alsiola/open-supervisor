package toolbelt

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// ScreenshotsBranch is where screenshots are committed, one folder per issue.
const ScreenshotsBranch = "open-supervisor-screenshots"

func run(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func screenshotsCmd(f *flags) *cobra.Command {
	return &cobra.Command{
		Use:   "screenshots <file>...",
		Short: "Commit images to the screenshots branch and print Markdown for each, pinned to the commit",
		Long: "Commits the images under issue-<key>/ on the " + ScreenshotsBranch + " branch, pushes, and prints a Markdown image line for each,\n" +
			"pinned to that commit so later pushes never change what a PR shows. Paste the lines into the PR body.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := open(f, true)
			if err != nil {
				return err
			}
			var files []string
			for _, a := range args {
				abs, err := filepath.Abs(a)
				if err != nil {
					return err
				}
				if _, err := os.Stat(abs); err != nil {
					return err
				}
				files = append(files, abs)
			}
			root := s.b.Root
			tmp, err := os.MkdirTemp("", "open-supervisor-shots-")
			if err != nil {
				return err
			}
			_ = os.Remove(tmp) // git worktree add wants to create it
			defer func() {
				_, _ = run(root, "worktree", "remove", "--force", tmp)
				_ = os.RemoveAll(tmp)
			}()
			if _, err := run(root, "fetch", "-q", "origin", ScreenshotsBranch); err == nil {
				if _, err := run(root, "worktree", "add", "-q", "--detach", tmp, "FETCH_HEAD"); err != nil {
					return err
				}
			} else {
				if _, err := run(root, "worktree", "add", "-q", "--detach", tmp); err != nil {
					return err
				}
				if _, err := run(tmp, "checkout", "-q", "--orphan", ScreenshotsBranch); err != nil {
					return err
				}
				_, _ = run(tmp, "rm", "-rfq", ".")
			}
			dir := "issue-" + strings.ReplaceAll(s.key, "/", "-")
			if err := os.MkdirAll(filepath.Join(tmp, dir), 0o755); err != nil {
				return err
			}
			for _, file := range files {
				data, err := os.ReadFile(file)
				if err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(tmp, dir, filepath.Base(file)), data, 0o644); err != nil {
					return err
				}
			}
			if _, err := run(tmp, "add", dir); err != nil {
				return err
			}
			if _, err := run(tmp, "diff", "--cached", "--quiet"); err != nil {
				if _, err := run(tmp, "-c", "commit.gpgsign=false", "commit", "-qm", "Screenshots for "+s.key); err != nil {
					return err
				}
			} else {
				fmt.Fprintln(cmd.ErrOrStderr(), "screenshots unchanged")
			}
			if s.dryRun {
				fmt.Fprintln(cmd.OutOrStdout(), "dry run: would push the screenshots")
				return nil
			}
			// Other agents push here too; their files are in other folders, so a rebase never conflicts.
			for attempt := 1; ; attempt++ {
				if _, err := run(tmp, "push", "-q", "origin", "HEAD:refs/heads/"+ScreenshotsBranch); err == nil {
					break
				} else if attempt == 5 {
					return err
				}
				_, _ = run(tmp, "fetch", "-q", "origin", ScreenshotsBranch)
				if _, err := run(tmp, "-c", "commit.gpgsign=false", "rebase", "-q", "FETCH_HEAD"); err != nil {
					return err
				}
			}
			sha, err := run(tmp, "rev-parse", "HEAD")
			if err != nil {
				return err
			}
			for _, file := range files {
				name := filepath.Base(file)
				fmt.Fprintf(cmd.OutOrStdout(), "![%s](%s)\n", strings.TrimSuffix(name, filepath.Ext(name)), s.b.Repo.RawURL(sha, dir+"/"+name))
			}
			return nil
		},
	}
}
