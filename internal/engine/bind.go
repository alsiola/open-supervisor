package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/config"
)

// Bound is a project's connections opened and bound to it: what both the engine and the agent toolbelt need.
type Bound struct {
	Spec           *config.Project
	Root           string
	StateDir       string
	Tracker        adapter.Tracker
	Repo           adapter.Repo
	Agent          adapter.Agent
	AgentName      string
	BranchTemplate string
	branchRe       *regexp.Regexp
}

// BranchFor is the issue's branch.
func (b *Bound) BranchFor(key string) string {
	return strings.ReplaceAll(b.BranchTemplate, "{key}", key)
}

// IssueOfBranch reads the issue key back out of a branch name, "" when it isn't an issue branch.
func (b *Bound) IssueOfBranch(branch string) string {
	if m := b.branchRe.FindStringSubmatch(branch); m != nil {
		return m[1]
	}
	return ""
}

// Bind opens a project's agent, repo and tracker. stateDir is the supervisor's state directory.
func Bind(cfg *config.Config, spec *config.Project, conns *Connections, stateDir string) (*Bound, error) {
	root := config.ExpandHome(spec.Root)
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		return nil, fmt.Errorf("%s isn't a git checkout", root)
	}
	if spec.Repo == nil || spec.Tracker == nil {
		return nil, fmt.Errorf("needs a repo and a tracker")
	}
	info := adapter.ProjectInfo{Name: spec.Name, Root: root, Remote: gitRemote(root)}
	agentName := cfg.AgentFor(spec)
	agentConn, err := conns.Get(agentName)
	if err != nil {
		return nil, err
	}
	agent, ok := agentConn.(adapter.Agent)
	if !ok {
		return nil, fmt.Errorf("%s can't run turns", agentName)
	}
	repoConn, err := conns.Get(spec.Repo.Connection)
	if err != nil {
		return nil, err
	}
	rp, ok := repoConn.(adapter.RepoProvider)
	if !ok {
		return nil, fmt.Errorf("%s can't host code", spec.Repo.Connection)
	}
	binding := spec.Repo.Values
	if detected, ok := rp.DetectRepo(info.Remote); ok {
		binding = merge(detected, binding)
	}
	repo, err := rp.Repo(binding, info)
	if err != nil {
		return nil, fmt.Errorf("repo: %w", err)
	}
	trackerConn, err := conns.Get(spec.Tracker.Connection)
	if err != nil {
		return nil, err
	}
	tp, ok := trackerConn.(adapter.TrackerProvider)
	if !ok {
		return nil, fmt.Errorf("%s can't hold issues", spec.Tracker.Connection)
	}
	tb := spec.Tracker.Values
	if spec.Tracker.Connection == spec.Repo.Connection {
		// A service that is both (GitHub) tracks the repo's own issues unless told otherwise.
		tb = merge(binding, tb)
	}
	tracker, err := tp.Tracker(tb, info)
	if err != nil {
		return nil, fmt.Errorf("tracker: %w", err)
	}
	tmpl := cfg.BranchFor(spec)
	if !strings.Contains(tmpl, "{key}") {
		return nil, fmt.Errorf("branch %q has no {key}", tmpl)
	}
	re := regexp.MustCompile("^" + strings.Replace(regexp.QuoteMeta(tmpl), regexp.QuoteMeta("{key}"), "(.+)", 1) + "$")
	return &Bound{
		Spec:           spec,
		Root:           root,
		StateDir:       filepath.Join(stateDir, "projects", spec.Name),
		Tracker:        tracker,
		Repo:           repo,
		Agent:          agent,
		AgentName:      agentName,
		BranchTemplate: tmpl,
		branchRe:       re,
	}, nil
}

// merge lays over's non-empty values over base.
func merge(base, over adapter.Values) adapter.Values {
	out := adapter.Values{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		if v != "" {
			out[k] = v
		}
	}
	return out
}
