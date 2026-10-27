// open-supervisor claims issues across your repos and works each in its own coding-agent session, one turn at a
// time, from a small TUI.
//
//	open-supervisor              run (the TUI; headless when there is no terminal)
//	open-supervisor init         the onboarding wizard
//	open-supervisor doctor       check the config, every connection and every project
//	open-supervisor agent …      the commands agents use inside their sessions
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/alsiola/open-supervisor/internal/adapter"
	_ "github.com/alsiola/open-supervisor/internal/adapters/all"
	"github.com/alsiola/open-supervisor/internal/config"
	"github.com/alsiola/open-supervisor/internal/configcli"
	"github.com/alsiola/open-supervisor/internal/core"
	"github.com/alsiola/open-supervisor/internal/engine"
	"github.com/alsiola/open-supervisor/internal/logx"
	"github.com/alsiola/open-supervisor/internal/playbooks"
	"github.com/alsiola/open-supervisor/internal/proc"
	"github.com/alsiola/open-supervisor/internal/toolbelt"
	"github.com/alsiola/open-supervisor/internal/tui"
	"github.com/alsiola/open-supervisor/internal/wizard"
)

// Set by the release build.
var (
	version = "dev"
	commit  = ""
)

func main() {
	if err := root().Execute(); err != nil {
		os.Exit(1)
	}
}

func root() *cobra.Command {
	var configPath string
	var headless, dryRun, noTunnel bool
	cmd := &cobra.Command{
		Use:   config.Name,
		Short: "Claims issues across your repos and works each in its own coding-agent session",
		Long: "open-supervisor watches the issue trackers of your projects, claims open issues, and works each one in its own\n" +
			"coding-agent session, a turn at a time: comments, check results and merges become the next turn's events.\n\n" +
			"Run it with no arguments for the TUI. The first run starts the setup wizard.",
		Version:      version,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(resolve(configPath), headless, dryRun, noTunnel)
		},
	}
	cmd.SetVersionTemplate("{{.Version}}\n")
	cmd.PersistentFlags().StringVar(&configPath, "config", "", "config file (default $OPEN_SUPERVISOR_CONFIG or ~/.config/open-supervisor/config.yaml)")
	cmd.Flags().BoolVar(&headless, "headless", false, "no TUI; the log goes to stdout (for launchd, systemd, a server)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "change nothing on any service and run no jobs; turns run a stub prompt on a cheap model")
	cmd.Flags().BoolVar(&noTunnel, "no-tunnel", false, "no webhooks; services are read only by the reconcile")

	var setupWith string
	initCmd := &cobra.Command{
		Use:   "init [--with <agent>]",
		Short: "Set up connections and projects with the onboarding wizard, or have your coding agent do it",
		Long: "Without --with, the onboarding wizard asks a step at a time.\n\n" +
			"--with claude-code (or an agent connection's name) starts that agent in this terminal with the setup guide:\n" +
			"it looks at your checkouts, asks about your services and how issues map to repos, and writes the config\n" +
			"through `" + config.Name + " config`. It's an ordinary interactive session: answer it, and stop it when you like.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := resolve(configPath)
			if setupWith != "" {
				return setupWithAgent(path, setupWith)
			}
			// Read, not Load: an incomplete config is what init is for.
			existing, err := config.Read(path)
			if err != nil && !errors.Is(err, config.ErrNoConfig) {
				return err
			}
			_, err = wizard.Run(path, existing)
			return err
		},
	}
	initCmd.Flags().StringVar(&setupWith, "with", "", "have this coding agent set things up (claude-code, or an agent connection)")
	doctorCmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the config, every connection and every project",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return doctor(resolve(configPath))
		},
	}
	pluginCmd := &cobra.Command{
		Use:   "playbooks",
		Short: "Write the agent playbooks out and print where (claude --plugin-dir <dir> to use them by hand)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return writePlaybooks(resolve(configPath))
		},
	}
	var routeSource string
	routeCmd := &cobra.Command{
		Use:   "route <issue>",
		Short: "Show an issue's properties and which project its source's routes send it to",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return explainRoute(resolve(configPath), routeSource, args[0])
		},
	}
	routeCmd.Flags().StringVar(&routeSource, "source", "", "the source to route through (or a project with a tracker of its own)")
	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			v := version
			if commit != "" {
				v += " (" + commit + ")"
			}
			fmt.Println(config.Name, v)
		},
	}
	cmd.AddCommand(initCmd, doctorCmd, routeCmd, pluginCmd, versionCmd, toolbelt.Command(), adaptersCmd(), configcli.Command(func() string { return resolve(configPath) }))
	return cmd
}

func resolve(path string) string {
	if path == "" {
		return config.DefaultPath()
	}
	return config.ExpandHome(path)
}

func interactive() bool {
	return isatty.IsTerminal(os.Stdout.Fd()) && isatty.IsTerminal(os.Stdin.Fd())
}

// lock makes sure one supervisor runs at a time: two would claim and run the same issues.
func lock() (func(), error) {
	dir := config.StateDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	file := filepath.Join(dir, "supervisor.pid")
	if data, err := os.ReadFile(file); err == nil {
		if pid, _ := strconv.Atoi(strings.TrimSpace(string(data))); pid > 0 && pid != os.Getpid() && proc.Alive(pid) {
			return nil, fmt.Errorf("a supervisor is already running (pid %d); stop it first", pid)
		}
	}
	if err := os.WriteFile(file, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return nil, err
	}
	return func() { _ = os.Remove(file) }, nil
}

func run(path string, headless, dryRun, noTunnel bool) error {
	headless = headless || !interactive()
	cfg, err := config.Load(path)
	if errors.Is(err, config.ErrNoConfig) && !headless {
		how, err2 := wizard.ChooseSetup()
		if err2 != nil {
			return err2
		}
		if how == "" {
			fmt.Println("No config yet: starting the setup wizard.")
			if cfg, err = wizard.Run(path, nil); err != nil {
				return err
			}
		} else {
			if err := setupWithAgent(path, how); err != nil {
				return err
			}
			if cfg, err = config.Load(path); err != nil {
				return fmt.Errorf("%w\nrun `%s init --with %s` to carry on, or `%s init` for the wizard", err, config.Name, how, config.Name)
			}
		}
	}
	if err != nil {
		if cfg != nil {
			return fmt.Errorf("%w\nfix it with `%s init` (or `init --with claude-code`), `%s config`, or by hand", err, config.Name, config.Name)
		}
		return err
	}
	if len(cfg.Projects) == 0 {
		return fmt.Errorf("%s has no projects yet: run `%s init` to add one", path, config.Name)
	}
	unlock, err := lock()
	if err != nil {
		return err
	}
	defer unlock()
	logx.Default.Open(filepath.Join(config.StateDir(), "log"), headless)

	sup, err := engine.New(cfg, engine.Options{DryRun: dryRun, NoTunnel: noTunnel})
	if err != nil {
		return err
	}
	if err := sup.Start(); err != nil {
		return err
	}
	defer sup.Stop()

	if headless {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		return nil
	}
	signal.Ignore(syscall.SIGINT) // the TUI takes ctrl+c itself
	return tui.Run(sup)
}

func adapterEnv(cfg *config.Config) adapter.Env {
	return adapter.Env{
		Log:     func(format string, args ...any) { fmt.Printf("    "+format+"\n", args...) },
		Secret:  config.ResolveSecret,
		DryRun:  true,
		DataDir: config.DataDir(),
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// doctor checks everything a run needs, saying what is wrong and how to fix it.
func doctor(path string) error {
	fmt.Printf("config     %s\n", path)
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Printf("  ✗ %v\n", err)
		return errors.New("fix the config first")
	}
	fmt.Printf("state      %s\ndata       %s\n\n", config.StateDir(), config.DataDir())
	failed := 0
	env := adapterEnv(cfg)
	names := make([]string, 0, len(cfg.Connections))
	for name := range cfg.Connections {
		names = append(names, name)
	}
	sort.Strings(names)
	fmt.Println("connections")
	conns := engine.OpenConnections(cfg, env)
	for _, name := range names {
		c := cfg.Connections[name]
		conn, err := conns.Get(name)
		if err != nil {
			failed++
			fmt.Printf("  ✗ %-14s %-12s %v\n", name, c.Type, err)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		who, err := conn.Check(ctx)
		cancel()
		if err != nil {
			failed++
			fmt.Printf("  ✗ %-14s %-12s %v\n", name, c.Type, err)
			continue
		}
		fmt.Printf("  ✓ %-14s %-12s %s\n", name, c.Type, who)
	}
	fmt.Println("\nprojects")
	for _, p := range cfg.Projects {
		if p.Disabled {
			fmt.Printf("  - %-14s disabled\n", p.Name)
			continue
		}
		b, err := engine.Bind(cfg, p, conns, config.StateDir())
		if err != nil {
			failed++
			fmt.Printf("  ✗ %-14s %v\n", p.Name, err)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		var issues []core.Issue
		if b.Source.Implicit() {
			issues, err = b.Tracker.Candidates(ctx)
		}
		var base string
		if err == nil {
			base = p.BaseBranch
			if base == "" {
				base, err = b.Repo.DefaultBranch(ctx)
			}
		}
		cancel()
		if err != nil {
			failed++
			fmt.Printf("  ✗ %-14s %s · %s: %v\n", p.Name, b.Tracker.Describe(), b.Repo.Describe(), err)
			continue
		}
		if !b.Source.Implicit() {
			fmt.Printf("  ✓ %-14s issues from %s · %s · base %s · agent %s\n", p.Name, b.Source.Name, b.Repo.Describe(), base, b.AgentName)
			continue
		}
		claimable := 0
		for _, i := range issues {
			if claimableIssue(i, cfg.LabelsFor(p)) {
				claimable++
			}
		}
		fmt.Printf("  ✓ %-14s %s · %s · base %s · %d open, %d claimable · agent %s\n", p.Name, b.Tracker.Describe(), b.Repo.Describe(), base, len(issues), claimable, b.AgentName)
	}
	if len(cfg.SourceList) > 0 {
		fmt.Println("\nsources")
	}
	for _, src := range cfg.SourceList {
		if !doctorSource(cfg, src, conns) {
			failed++
		}
	}
	if cfg.Settings.Tunnel == "" {
		fmt.Printf("\nwebhooks   off: the reconcile reads every %d minutes\n", cfg.Settings.ReconcileMinutes)
	} else {
		fmt.Printf("\nwebhooks   through %s, to 127.0.0.1:%d\n", cfg.Settings.Tunnel, cfg.Settings.Port)
	}
	if failed > 0 {
		return fmt.Errorf("%d problem(s)", failed)
	}
	fmt.Println("\nall good: run `open-supervisor`")
	return nil
}

// claimableIssue reports whether the supervisor would claim the issue, routes aside.
func claimableIssue(i core.Issue, labels config.Labels) bool {
	if !i.Open || i.Waiting || i.HasLabel(labels.Claimed) {
		return false
	}
	return labels.Admits(i.HasLabel)
}

// doctorSource reads a source's open issues and says where its routes send them.
func doctorSource(cfg *config.Config, src *config.Source, conns *engine.Connections) bool {
	tracker, err := engine.BindSource(cfg, src, conns)
	if err != nil {
		fmt.Printf("  ✗ %-14s %v\n", src.Name, err)
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	issues, err := tracker.Candidates(ctx)
	cancel()
	if err != nil {
		fmt.Printf("  ✗ %-14s %s: %v\n", src.Name, tracker.Describe(), err)
		return false
	}
	labels := cfg.SourceLabels(src)
	counts := make([]int, len(src.Routes))
	claimable, unrouted := 0, 0
	for _, i := range issues {
		if !claimableIssue(i, labels) {
			continue
		}
		claimable++
		if n := src.RouteFor(i.Attr); n >= 0 {
			counts[n]++
		} else {
			unrouted++
		}
	}
	fmt.Printf("  ✓ %-14s %s · %d open, %d claimable, %d unrouted\n", src.Name, tracker.Describe(), len(issues), claimable, unrouted)
	for n, r := range src.Routes {
		note := ""
		if counts[n] == 0 && claimable > 0 {
			note = "  (takes nothing open now)"
		}
		fmt.Printf("      %d. %-12s %3d  when %s%s\n", n+1, r.Project, counts[n], r.When.String(), note)
	}
	return true
}

// explainRoute reads one issue and shows its properties and which of its source's routes take it.
func explainRoute(path, sourceName, key string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	var src *config.Source
	switch {
	case sourceName != "":
		if src = cfg.FindSource(sourceName); src == nil {
			return fmt.Errorf("no source or project with a tracker named %q", sourceName)
		}
	case len(cfg.SourceList) == 1:
		src = cfg.SourceList[0]
	default:
		names := []string{}
		for _, s := range cfg.Sources() {
			names = append(names, s.Name)
		}
		return fmt.Errorf("which source? pass --source (one of %s)", strings.Join(names, ", "))
	}
	tracker, err := engine.BindSource(cfg, src, engine.OpenConnections(cfg, adapterEnv(cfg)))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	issue, err := tracker.Issue(ctx, strings.TrimPrefix(key, "#"))
	if err != nil {
		return err
	}
	fmt.Printf("%s %s\n%s\n\n", issue.Key, issue.Title, issue.URL)
	props := map[string][]string{"labels": issue.Labels}
	for k, v := range issue.Attrs {
		props[k] = v
	}
	names := make([]string, 0, len(props))
	for k := range props {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		fmt.Printf("  %-14s %s\n", k, strings.Join(props[k], ", "))
	}
	fmt.Printf("\nroutes of %s\n", src.Name)
	n := src.RouteFor(issue.Attr)
	for i, r := range src.Routes {
		mark := "✗"
		switch {
		case i == n:
			mark = "✓"
		case n >= 0 && i > n:
			mark = " "
		}
		fmt.Printf("  %s %d. %-12s when %s\n", mark, i+1, r.Project, r.When.String())
	}
	labels := cfg.SourceLabels(src)
	switch {
	case n >= 0 && issue.Waiting:
		fmt.Printf("\n→ %s, once it moves to a state %s claims from\n", src.Routes[n].Project, tracker.Describe())
	case n >= 0 && !claimableIssue(issue, labels):
		fmt.Printf("\n→ %s, once it is open and carries neither the claimed label nor a skip label\n", src.Routes[n].Project)
	case n >= 0:
		fmt.Printf("\n→ %s\n", src.Routes[n].Project)
	default:
		fmt.Printf("\n→ no route takes it (unrouted: %s)\n", orDefault(src.Unrouted, config.UnroutedIgnore))
	}
	return nil
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func writePlaybooks(path string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	env := adapterEnv(cfg)
	for _, name := range cfg.ConnectionNames(adapter.RoleAgent) {
		conn, err := engine.Open(cfg, name, env)
		if err != nil {
			return err
		}
		agent := conn.(adapter.Agent)
		if err := agent.Prepare(context.Background(), playbooks.All(cfg.Settings.Ambient.Mode())); err != nil {
			fmt.Printf("%s: %v\n", name, err)
		}
		if d, ok := conn.(interface{ PluginDir() string }); ok {
			fmt.Printf("%s: %s\n", name, d.PluginDir())
		}
	}
	return nil
}

func adaptersCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "adapters [<type>]",
		Short: "List the adapters this build has and the roles each can fill, or one adapter's fields",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				s, ok := adapter.Lookup(args[0])
				if !ok {
					return fmt.Errorf("no adapter %q", args[0])
				}
				describeAdapter(s)
				return nil
			}
			for _, s := range adapter.All() {
				roles := make([]string, len(s.Roles))
				for i, r := range s.Roles {
					roles[i] = string(r)
				}
				fmt.Printf("%-12s %-18s %-24s %s\n", s.Type, s.Title, strings.Join(roles, ", "), s.Description)
			}
			return nil
		},
	}
}

// describeAdapter prints an adapter's connection fields and, per role, the fields a project or source binds.
func describeAdapter(s adapter.Spec) {
	roles := make([]string, len(s.Roles))
	for i, r := range s.Roles {
		roles[i] = string(r)
	}
	fmt.Printf("%s (%s): %s\nroles: %s\n", s.Type, s.Title, s.Description, strings.Join(roles, ", "))
	fields := func(title string, fs []adapter.Field) {
		if len(fs) == 0 {
			return
		}
		fmt.Printf("\n%s\n", title)
		for _, f := range fs {
			var notes []string
			if f.Required {
				notes = append(notes, "required")
			}
			notes = append(notes, string(f.Type))
			if f.Default != "" {
				notes = append(notes, "default "+f.Default)
			}
			if len(f.Options) > 0 {
				notes = append(notes, "one of "+strings.Join(f.Options, ", "))
			}
			for k, v := range f.ShowIf {
				notes = append(notes, "when "+k+" is "+strings.Join(v, " or "))
			}
			fmt.Printf("  %-16s %s (%s)\n", f.Key, f.Label, strings.Join(notes, "; "))
			if f.Help != "" {
				fmt.Printf("  %-16s %s\n", "", f.Help)
			}
		}
	}
	fields("connection fields (config connection set <name> --type "+s.Type+" --set key=value)", s.Fields)
	for _, r := range s.Roles {
		flag := map[adapter.Role]string{adapter.RoleRepo: "config project set --repo key=value", adapter.RoleTracker: "config project set --tracker key=value, or config source set --set key=value"}[r]
		if flag == "" {
			flag = "bound by " + string(r)
		}
		fields(string(r)+" binding fields ("+flag+")", s.Binding[r])
	}
}

// setupWithAgent starts a coding agent in this terminal with the setup guide. agent is an agent connection in
// the config, or an agent adapter's type, opened with its defaults.
func setupWithAgent(path, agent string) error {
	cfg, err := config.Read(path)
	if errors.Is(err, config.ErrNoConfig) {
		cfg = config.New(path)
	} else if err != nil {
		return err
	}
	env := adapterEnv(cfg)
	env.Log = func(string, ...any) {}
	if _, ok := cfg.Connections[agent]; !ok && agent == "claude" {
		agent = "claude-code"
	}
	var conn adapter.Connection
	if c, ok := cfg.Connections[agent]; ok {
		if spec, ok := adapter.Lookup(c.Type); !ok || !spec.Has(adapter.RoleAgent) {
			return fmt.Errorf("%s isn't a coding agent", agent)
		}
		conn, err = engine.Open(cfg, agent, env)
	} else if spec, ok := adapter.Lookup(agent); ok && spec.Has(adapter.RoleAgent) {
		conn, err = spec.New(spec.Defaults(), env)
	} else {
		var names []string
		for _, s := range adapter.ForRole(adapter.RoleAgent) {
			names = append(names, s.Type)
		}
		return fmt.Errorf("no coding agent %q: one of %s, or an agent connection's name", agent, strings.Join(names, ", "))
	}
	if err != nil {
		return err
	}
	sa, ok := conn.(adapter.SetupAgent)
	if !ok {
		return fmt.Errorf("%s can't run a setup session; run `%s init` for the wizard", agent, config.Name)
	}
	bin := selfCommand()
	cwd, _ := os.Getwd()
	cmd, err := sa.SetupCommand(adapter.SetupSpec{
		Cwd:   cwd,
		Env:   append(os.Environ(), "OPEN_SUPERVISOR_CONFIG="+path),
		Guide: playbooks.Setup(bin),
		// Reading and changing the supervisor's own config, nothing else, goes without asking.
		Allow: []string{bin + " config", bin + " adapters", bin + " doctor", bin + " route"},
	})
	if err != nil {
		return err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	fmt.Printf("Starting %s to set up %s (config %s)…\n", agent, config.Name, path)
	return cmd.Run()
}

// selfCommand is how an agent should run this binary: its name when that's what PATH finds, else its path.
func selfCommand() string {
	self, err := os.Executable()
	if err != nil {
		return config.Name
	}
	self, _ = filepath.EvalSymlinks(self)
	if found, err := exec.LookPath(config.Name); err == nil {
		if f, err := filepath.EvalSymlinks(found); err == nil && f == self {
			return config.Name
		}
	}
	return self
}
