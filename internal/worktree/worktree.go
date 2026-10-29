// Package worktree gives each issue its own git worktree on its own branch, outside the project's checkout so
// nothing has to be ignored there. Setup makes the .local/ scratch folder the playbook points agents at, copies
// the files the project lists (.env) on every run and runs the project's setup command when one of the files it
// watches (a lockfile) has changed; Refresh copies the files alone, before each turn, so a resumed agent that
// doesn't run Setup again still sees keys added since. Setup won't make a new worktree on a disk it would leave
// too full, and clears away one git left half-made. Remove stops anything still running from the worktree,
// removes it and keeps the branch, so nothing committed is lost.
package worktree

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/alsiola/open-supervisor/internal/host"
)

// Spec is one issue's worktree.
type Spec struct {
	// Root is the project's main checkout.
	Root string
	// Path is where the worktree goes.
	Path   string
	Branch string
	// Remote is the remote to fetch from; empty is origin.
	Remote string
	// Base is the branch a new one starts from.
	Base      string
	Copy      []string
	Setup     string
	SetupWhen []string
	// Disk is the host's disk threshold, the percentage in use over which a disk is running out. Setup doesn't
	// make a new worktree whose checkout would leave less free than the threshold keeps (at most MaxReserve).
	// 0 checks nothing.
	Disk float64
	// Log receives what happened, a line at a time.
	Log io.Writer
}

var unsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// Dir is where a project's issue worktree lives under the data directory.
func Dir(dataDir, project, key string) string {
	return filepath.Join(dataDir, "worktrees", project, "issue-"+unsafe.ReplaceAllString(key, "-"))
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// remote is the remote to fetch from.
func (s Spec) remote() string {
	if s.Remote != "" {
		return s.Remote
	}
	return "origin"
}

func (s Spec) logf(format string, args ...any) {
	if s.Log != nil {
		fmt.Fprintf(s.Log, format+"\n", args...)
	}
}

// Setup creates or reuses the worktree and returns its path.
func Setup(s Spec) (string, error) {
	if _, err := git(s.Root, "fetch", "-q", s.remote()); err != nil {
		s.logf("fetch failed, carrying on: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return "", err
	}
	whole, err := s.checkedOut()
	if err != nil {
		return "", err
	}
	if whole {
		s.logf("reusing %s", s.Path)
	} else {
		// A worktree registered at a folder that has since gone stops git adding one there again.
		_, _ = git(s.Root, "worktree", "prune")
		// start is what the checkout is made from; tracking, a remote branch it follows.
		var start, tracking string
		var args []string
		switch {
		case refExists(s.Root, "refs/heads/"+s.Branch):
			start, args = s.Branch, []string{s.Path, s.Branch}
		case refExists(s.Root, "refs/remotes/"+s.remote()+"/"+s.Branch):
			start = s.remote() + "/" + s.Branch
			tracking, args = start, []string{"-b", s.Branch, s.Path, start}
		default:
			base := s.Base
			if base == "" {
				base = "main"
			}
			start = s.remote() + "/" + base
			if !refExists(s.Root, "refs/remotes/"+start) {
				start = base
			}
			args = []string{"--no-track", "-b", s.Branch, s.Path, start}
		}
		if err := s.roomFor(start); err != nil {
			return "", err
		}
		if _, err := git(s.Root, append([]string{"worktree", "add", "-q"}, args...)...); err != nil {
			// A checkout that failed part-way (a full disk) would be reused next time as if it were whole.
			_ = os.RemoveAll(s.Path)
			_, _ = git(s.Root, "worktree", "prune")
			return "", err
		}
		if tracking != "" {
			_, _ = git(s.Path, "branch", "-q", "--set-upstream-to", tracking)
		}
	}
	s.scratch()
	// Copied on every run, not only when missing, so a reused worktree picks up keys added since.
	s.copyFiles()
	if s.Setup != "" {
		if err := runSetup(s); err != nil {
			return s.Path, err
		}
	}
	return s.Path, nil
}

// checkedOut reports whether the worktree's folder is a checkout of its own. A folder with no checkout in it
// (only files copied in, or a cache a dev server left running from it wrote) is cleared so Setup can add the
// worktree again; it would otherwise be reused as one, with nothing in it. A folder git once checked out to but
// no longer knows is repaired, or left for someone to look at, since it may hold uncommitted work.
func (s Spec) checkedOut() (bool, error) {
	if _, err := os.Stat(s.Path); err != nil {
		return false, nil
	}
	if s.toplevel() {
		return true, nil
	}
	if _, err := os.Lstat(filepath.Join(s.Path, ".git")); err == nil {
		_, _ = git(s.Root, "worktree", "repair", s.Path)
		if s.toplevel() {
			s.logf("repaired the worktree at %s", s.Path)
			return true, nil
		}
		return false, fmt.Errorf("%s has a .git but isn't a worktree git can use; look at it, remove it and run setup again", s.Path)
	}
	s.logf("%s isn't a checkout; clearing it and adding the worktree again", s.Path)
	killWithin(s.Path)
	if err := os.RemoveAll(s.Path); err != nil {
		return false, fmt.Errorf("clearing %s, which isn't a checkout: %w", s.Path, err)
	}
	return false, nil
}

// toplevel reports whether the worktree's folder is the top of a git checkout, not a folder inside another.
func (s Spec) toplevel() bool {
	top, err := git(s.Path, "rev-parse", "--show-toplevel")
	if err != nil {
		return false
	}
	return samePath(top, s.Path)
}

func samePath(a, b string) bool {
	x, err1 := filepath.EvalSymlinks(a)
	y, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return x == y
}

// Scratch is the folder, relative to the worktree, where agents keep files they don't commit.
const Scratch = ".local"

// scratch makes the worktree's scratch folder, so the first redirect into it doesn't fail. A project that
// doesn't ignore it sees nothing until something is put there: git doesn't track an empty folder.
func (s Spec) scratch() {
	if err := os.MkdirAll(filepath.Join(s.Path, Scratch), 0o755); err != nil {
		s.logf("couldn't make %s: %v", Scratch, err)
	}
}

// MaxReserve is the most free space Setup keeps on a disk, so a large one isn't held at its threshold's share.
var MaxReserve uint64 = 10 << 30

// NoRoom is a new worktree that would leave its disk too full.
type NoRoom struct {
	Path string
	// Free is what the disk has, Need what the checkout takes and Keep what must be left after it, in bytes.
	Free, Need, Keep uint64
}

func (e *NoRoom) Error() string {
	return fmt.Sprintf("not enough disk space for a new worktree at %s: the disk has %s free, and the checkout needs about %s on top of the %s kept free (settings.host.disk)",
		e.Path, Bytes(e.Free), Bytes(e.Need), Bytes(e.Keep))
}

// roomFor checks the worktree's disk has room for a checkout of ref. A disk it can't read doesn't stop it.
func (s Spec) roomFor(ref string) error {
	if s.Disk <= 0 || s.Disk >= 100 {
		return nil
	}
	d, err := host.DiskAt(s.Path)
	if err != nil || d.Total == 0 {
		return nil
	}
	need := checkoutSize(s.Root, ref)
	keep := min(uint64(float64(d.Total)*(100-s.Disk)/100), MaxReserve)
	if d.Free >= need+keep {
		return nil
	}
	return &NoRoom{Path: s.Path, Free: d.Free, Need: need, Keep: keep}
}

// checkoutSize is how many bytes ref's files take, 0 when git can't say.
func checkoutSize(root, ref string) uint64 {
	out, err := exec.Command("git", "-C", root, "ls-tree", "-r", "-l", "-z", ref).Output()
	if err != nil {
		return 0
	}
	var total uint64
	for _, entry := range strings.Split(string(out), "\x00") {
		meta, _, _ := strings.Cut(entry, "\t")
		if f := strings.Fields(meta); len(f) == 4 {
			n, _ := strconv.ParseUint(f[3], 10, 64)
			total += n
		}
	}
	return total
}

// Bytes is a size for people to read: "1.4 GB".
func Bytes(n uint64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
}

// Refresh copies the project's files into the worktree again when it exists, so a key added to the main
// checkout's .env since Setup last ran reaches it. It fetches nothing and runs no setup.
func Refresh(s Spec) {
	if _, err := os.Stat(filepath.Join(s.Path, ".git")); err != nil {
		return
	}
	s.copyFiles()
}

func (s Spec) copyFiles() {
	for _, name := range s.Copy {
		from := filepath.Join(s.Root, name)
		to := filepath.Join(s.Path, name)
		if same(from, to) {
			continue
		}
		if err := copyFile(from, to); err == nil {
			s.logf("copied %s", name)
		}
	}
}

// Fresh makes a new worktree at the base branch's remote head, detached, for one run of a job; it copies the
// project's files into it and runs its setup whatever has changed. On a setup failure it returns the path
// with the error, so the worktree can be kept to look at.
func Fresh(s Spec) (string, error) {
	base := s.Base
	if base == "" {
		base = "main"
	}
	if _, err := git(s.Root, "fetch", "-q", s.remote(), base); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return "", err
	}
	if err := s.roomFor(s.remote() + "/" + base); err != nil {
		return "", err
	}
	if _, err := git(s.Root, "worktree", "add", "-q", "--detach", s.Path, s.remote()+"/"+base); err != nil {
		_ = os.RemoveAll(s.Path)
		_, _ = git(s.Root, "worktree", "prune")
		return "", err
	}
	if head, err := git(s.Path, "rev-parse", "--short", "HEAD"); err == nil {
		s.logf("checked out %s/%s at %s in %s", s.remote(), base, head, s.Path)
	}
	s.scratch()
	s.copyFiles()
	if s.Setup != "" {
		if err := runSetup(s); err != nil {
			return s.Path, err
		}
	}
	return s.Path, nil
}

// Sync is how the main checkout stood against the base branch's remote head before a job ran in it.
type Sync struct {
	// Updated is how many commits it was fast-forwarded by.
	Updated int
	// Behind is how many commits of the remote head it still didn't have, and Stale why ("on branch x").
	// Stale is also set when the fetch failed, Behind then counting from the last fetch.
	Behind int
	Stale  string
}

// CatchUp fetches the base branch into the main checkout (Root) and fast-forwards it when it's on that branch.
// Uncommitted changes don't stop it: git leaves them be, and refuses when the update would touch one. A
// checkout without the remote has nothing to catch up with.
func CatchUp(s Spec) Sync {
	got := Behind(s)
	if got.Behind == 0 || got.Stale != "" {
		return got
	}
	base, head := s.head()
	branch, _ := git(s.Root, "symbolic-ref", "-q", "--short", "HEAD")
	if branch != base {
		if branch == "" {
			branch = "a detached HEAD"
		}
		got.Stale = "on " + branch + ", not " + base
		return got
	}
	if _, err := git(s.Root, "merge", "-q", "--ff-only", head); err != nil {
		switch msg := err.Error(); {
		case strings.Contains(msg, "would be overwritten"):
			got.Stale = "uncommitted changes to files the update touches"
		case strings.Contains(msg, "fast-forward") || strings.Contains(msg, "diverg"):
			got.Stale = base + " has commits " + head + " doesn't"
		default:
			got.Stale = "couldn't fast-forward: " + firstLine(msg)
		}
		return got
	}
	got.Updated, got.Behind = got.Behind, 0
	return got
}

// Behind fetches the base branch into the main checkout (Root) and counts the commits of its remote head that
// the checkout doesn't have, changing nothing else.
func Behind(s Spec) Sync {
	if _, err := git(s.Root, "remote", "get-url", s.remote()); err != nil {
		return Sync{}
	}
	var got Sync
	base, head := s.head()
	if _, err := git(s.Root, "fetch", "-q", s.remote(), base); err != nil {
		got.Stale = "couldn't fetch: " + firstLine(err.Error())
	}
	if !refExists(s.Root, "refs/remotes/"+head) {
		return got
	}
	if count, err := git(s.Root, "rev-list", "--count", "HEAD.."+head); err == nil {
		got.Behind, _ = strconv.Atoi(count)
	}
	return got
}

// head is the base branch and its remote-tracking ref.
func (s Spec) head() (string, string) {
	base := s.Base
	if base == "" {
		base = "main"
	}
	return base, s.remote() + "/" + base
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func refExists(dir, ref string) bool {
	_, err := git(dir, "show-ref", "--verify", "-q", ref)
	return err == nil
}

// runSetup runs the setup command when what it depends on has changed since it last ran. When it watches
// files and the checkout has none of them (a pnpm install in a repo with no pnpm-lock.yaml), it doesn't run.
func runSetup(s Spec) error {
	h := sha256.New()
	h.Write([]byte(s.Setup))
	found := false
	for _, name := range s.SetupWhen {
		if data, err := os.ReadFile(filepath.Join(s.Path, name)); err == nil {
			h.Write(data)
			found = true
		}
	}
	if len(s.SetupWhen) > 0 && !found {
		s.logf("skipping setup: none of %s is in the checkout", strings.Join(s.SetupWhen, ", "))
		return nil
	}
	sum := hex.EncodeToString(h.Sum(nil))
	stamp := filepath.Join(gitDir(s.Path), "open-supervisor-setup")
	if old, err := os.ReadFile(stamp); err == nil && strings.TrimSpace(string(old)) == sum {
		return nil
	}
	s.logf("running setup: %s", s.Setup)
	cmd := exec.Command("sh", "-c", s.Setup)
	cmd.Dir = s.Path
	cmd.Stdout = s.Log
	cmd.Stderr = s.Log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("setup (%s): %w", s.Setup, err)
	}
	return os.WriteFile(stamp, []byte(sum+"\n"), 0o644)
}

// gitDir is the worktree's own git directory, where the setup stamp is kept out of the work tree.
func gitDir(path string) string {
	if d, err := git(path, "rev-parse", "--absolute-git-dir"); err == nil {
		return d
	}
	return path
}

// Remove stops anything running from the worktree and removes it. The branch stays.
func Remove(s Spec) error {
	if _, err := os.Stat(s.Path); err != nil {
		s.logf("no worktree at %s", s.Path)
		return nil
	}
	killWithin(s.Path)
	_, _ = git(s.Root, "worktree", "remove", "--force", s.Path)
	if err := os.RemoveAll(s.Path); err != nil {
		return err
	}
	_, _ = git(s.Root, "worktree", "prune")
	s.logf("removed %s", s.Path)
	return nil
}

// Gone reports whether an issue's worktree was made and has since been removed: its branch is there and the
// worktree isn't.
func Gone(root, path, branch string) bool {
	if _, err := os.Stat(path); err == nil {
		return false
	}
	return refExists(root, "refs/heads/"+branch)
}

// Unpushed says what removing the worktree would lose: uncommitted changes to tracked or new files (ignored
// ones, like .local/, don't count), or commits no remote-tracking branch has. It is "" when there is nothing.
func Unpushed(s Spec) (string, error) {
	if _, err := os.Stat(s.Path); err != nil {
		return "", nil
	}
	status, err := git(s.Path, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	if status != "" {
		return "it has uncommitted changes", nil
	}
	count, err := git(s.Path, "rev-list", "--count", "HEAD", "--not", "--remotes="+s.remote())
	if err != nil {
		return "", err
	}
	if n, _ := strconv.Atoi(count); n > 0 {
		return fmt.Sprintf("%d commit(s) on it aren't pushed", n), nil
	}
	return "", nil
}

func same(a, b string) bool {
	x, err1 := os.ReadFile(a)
	y, err2 := os.ReadFile(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

func copyFile(from, to string) error {
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	info, err := os.Stat(from)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	return os.WriteFile(to, data, info.Mode().Perm())
}
