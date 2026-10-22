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
	Spec *config.Project
	// Source is where the project's issues come from: its own tracker, or a source with a route to it.
	Source         *config.Source
	Root           string
	StateDir       string
	Tracker        adapter.Tracker
	Repo           adapter.Repo
	Agent          adapter.Agent
	AgentName      string
	BranchTemplate string
	Git            config.Git
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

// Bind opens a project's agent, repo and tracker (its own, or its source's). stateDir is the supervisor's state
// directory.
func Bind(cfg *config.Config, spec *config.Project, conns *Connections, stateDir string) (*Bound, error) {
	root := config.ExpandHome(spec.Root)
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		return nil, fmt.Errorf("%s isn't a git checkout", root)
	}
	if spec.Repo == nil {
		return nil, fmt.Errorf("needs a repo")
	}
	src := cfg.SourceFor(spec)
	if src == nil {
		return nil, fmt.Errorf("needs a tracker, or a source with a route to it")
	}
	git := cfg.GitFor(spec)
	info := adapter.ProjectInfo{Name: spec.Name, Root: root, Remote: gitRemote(root, git.RemoteName())}
	agentName := cfg.AgentFor(spec)
	agentConn, err := conns.Get(agentName)
	if err != nil {
		return nil, err
	}
	agent, ok := agentConn.(adapter.Agent)
	if !ok {
		return nil, fmt.Errorf("%s can't run turns", agentName)
	}
	repo, binding, err := bindRepo(spec, conns, info)
	if err != nil {
		return nil, err
	}
	tracker, err := bindTracker(src, conns, spec, binding, info)
	if err != nil {
		return nil, err
	}
	tmpl := cfg.BranchFor(spec)
	if !strings.Contains(tmpl, "{key}") {
		return nil, fmt.Errorf("branch %q has no {key}", tmpl)
	}
	re := regexp.MustCompile("^" + strings.Replace(regexp.QuoteMeta(tmpl), regexp.QuoteMeta("{key}"), "(.+)", 1) + "$")
	return &Bound{
		Spec:           spec,
		Source:         src,
		Root:           root,
		StateDir:       filepath.Join(stateDir, "projects", spec.Name),
		Tracker:        tracker,
		Repo:           repo,
		Agent:          agent,
		AgentName:      agentName,
		BranchTemplate: tmpl,
		Git:            git,
		branchRe:       re,
	}, nil
}

// bindRepo opens a project's repo, with its binding filled from the checkout's remote where the service can.
func bindRepo(spec *config.Project, conns *Connections, info adapter.ProjectInfo) (adapter.Repo, adapter.Values, error) {
	repoConn, err := conns.Get(spec.Repo.Connection)
	if err != nil {
		return nil, nil, err
	}
	rp, ok := repoConn.(adapter.RepoProvider)
	if !ok {
		return nil, nil, fmt.Errorf("%s can't host code", spec.Repo.Connection)
	}
	binding := spec.Repo.Values
	if detected, ok := rp.DetectRepo(info.Remote); ok {
		binding = merge(detected, binding)
	}
	repo, err := rp.Repo(binding, info)
	if err != nil {
		return nil, nil, fmt.Errorf("repo: %w", err)
	}
	return repo, binding, nil
}

// bindTracker opens a source's tracker. A project's own tracker on the same service as its repo (GitHub) tracks
// the repo's own issues unless told otherwise, so it takes the repo's binding underneath its own.
func bindTracker(src *config.Source, conns *Connections, project *config.Project, repoBinding adapter.Values, info adapter.ProjectInfo) (adapter.Tracker, error) {
	trackerConn, err := conns.Get(src.Tracker.Connection)
	if err != nil {
		return nil, err
	}
	tp, ok := trackerConn.(adapter.TrackerProvider)
	if !ok {
		return nil, fmt.Errorf("%s can't hold issues", src.Tracker.Connection)
	}
	tb := src.Tracker.Values
	if src.Implicit() && project != nil && project.Repo != nil && src.Tracker.Connection == project.Repo.Connection {
		tb = merge(repoBinding, tb)
	}
	if !src.Implicit() {
		// A source belongs to no one checkout.
		info = adapter.ProjectInfo{Name: src.Name, Shared: true}
	}
	tracker, err := tp.Tracker(tb, info)
	if err != nil {
		return nil, fmt.Errorf("tracker: %w", err)
	}
	return tracker, nil
}

// BindSource opens a source's tracker on its own, for `doctor` and `route`.
func BindSource(cfg *config.Config, src *config.Source, conns *Connections) (adapter.Tracker, error) {
	if !src.Implicit() {
		return bindTracker(src, conns, nil, nil, adapter.ProjectInfo{})
	}
	spec := cfg.Project(src.Name)
	root := config.ExpandHome(spec.Root)
	info := adapter.ProjectInfo{Name: spec.Name, Root: root, Remote: gitRemote(root, cfg.GitFor(spec).RemoteName())}
	var binding adapter.Values
	if spec.Repo != nil && spec.Repo.Connection == src.Tracker.Connection {
		_, b, err := bindRepo(spec, conns, info)
		if err != nil {
			return nil, err
		}
		binding = b
	}
	return bindTracker(src, conns, spec, binding, info)
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
