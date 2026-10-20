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

	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Set up connections and projects with the onboarding wizard",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := resolve(configPath)
			existing, err := config.Load(path)
			if err != nil && !errors.Is(err, config.ErrNoConfig) {
				return err
			}
			_, err = wizard.Run(path, existing)
			return err
		},
	}
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
	cmd.AddCommand(initCmd, doctorCmd, pluginCmd, versionCmd, toolbelt.Command(), adaptersCmd())
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
		fmt.Println("No config yet: starting the setup wizard.")
		if cfg, err = wizard.Run(path, nil); err != nil {
			return err
		}
	}
	if err != nil {
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
		issues, err := b.Tracker.Candidates(ctx)
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
		labels := cfg.LabelsFor(p)
		claimable := 0
		for _, i := range issues {
			skip := i.HasLabel(labels.Claimed)
			for _, s := range labels.Skip {
				skip = skip || i.HasLabel(s)
			}
			if !skip {
				claimable++
			}
		}
		fmt.Printf("  ✓ %-14s %s · %s · base %s · %d open, %d claimable · agent %s\n", p.Name, b.Tracker.Describe(), b.Repo.Describe(), base, len(issues), claimable, b.AgentName)
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
		if err := agent.Prepare(context.Background(), playbooks.All()); err != nil {
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
		Use:   "adapters",
		Short: "List the adapters this build has, and the roles each can fill",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			for _, s := range adapter.All() {
				roles := make([]string, len(s.Roles))
				for i, r := range s.Roles {
					roles[i] = string(r)
				}
				fmt.Printf("%-12s %-18s %-24s %s\n", s.Type, s.Title, strings.Join(roles, ", "), s.Description)
			}
		},
	}
}
