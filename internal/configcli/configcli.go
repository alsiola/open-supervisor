// Package configcli is `open-supervisor config`: the config changed a step at a time from the command line, for
// scripts and for an agent setting the supervisor up. It is the wizard's and the TUI editor's drafts without
// the forms: a connection's fields are its adapter's, a project's repo is detected from its checkout, and
// secrets go to the keyring rather than the file. Every change is saved at once, and what the config still
// lacks is printed as a warning, since a config built a step at a time is incomplete in between.
package configcli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/config"
	"github.com/alsiola/open-supervisor/internal/forms"
)

// Command is `config` and its subcommands. path is the config file, resolved when a command runs.
func Command(path func() string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show and change the config from the command line (for scripts, and agents setting it up)",
		Long: "Changes the config a step at a time. Each command saves at once and then lists what the config still\n" +
			"lacks, so a setup can be built up in any order. `" + config.Name + " doctor` checks every connection when done.\n\n" +
			"Secrets never go in the file: a secret field takes a reference (env:NAME, cmd:..., keyring:..., file:...),\n" +
			"or a value, which is stored in the OS keyring. `config secret` reads one from the terminal, unseen.",
	}
	cmd.AddCommand(showCmd(path), pathCmd(path), getCmd(path), setCmd(path), unsetCmd(path),
		connectionCmd(path), secretCmd(path), projectCmd(path), sourceCmd(path), routeCmd(path))
	return cmd
}

// load reads the config, or starts a new one when there is none.
func load(path string) (*config.Config, error) {
	cfg, err := config.Read(path)
	if errors.Is(err, config.ErrNoConfig) {
		return config.New(path), nil
	}
	return cfg, err
}

// save writes the config and says what it still lacks.
func save(cmd *cobra.Command, cfg *config.Config, done string) error {
	if err := cfg.Save(); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if done != "" {
		fmt.Fprintln(out, "✓ "+done)
	}
	stillToDo(cmd, cfg)
	return nil
}

// stillToDo prints what the config lacks before the supervisor can run, if anything.
func stillToDo(cmd *cobra.Command, cfg *config.Config) {
	if len(cfg.Projects) == 0 && len(cfg.Connections) == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "still to do: everything (the config is empty)")
		return
	}
	if err := cfg.Validate(); err != nil {
		msg := strings.TrimPrefix(err.Error(), cfg.Path+":")
		fmt.Fprintln(cmd.ErrOrStderr(), "still to do:"+strings.ReplaceAll("\n"+strings.TrimSpace(msg), "\n", "\n  "))
	} else if len(cfg.Projects) == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "still to do: add a project")
	}
}

// values parses repeated key=value flags.
func values(pairs []string) (adapter.Values, error) {
	out := adapter.Values{}
	for _, kv := range pairs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("%q: want key=value", kv)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}

// setFields puts values into a field set, refusing keys it doesn't have.
func setFields(fs *forms.FieldSet, v adapter.Values, what string) error {
	for k, x := range v {
		found := false
		for _, f := range fs.Fields {
			found = found || f.Key == k
		}
		if !found {
			var keys []string
			for _, f := range fs.Fields {
				keys = append(keys, f.Key)
			}
			return fmt.Errorf("%s has no field %q (it has: %s)", what, k, strings.Join(keys, ", "))
		}
		fs.Set(k, x)
	}
	return fs.Validate()
}

// ---- the whole config ----

func showCmd(path func() string) *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print the config as the supervisor reads it, defaults included, and what it still lacks",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := load(path())
			if err != nil {
				return err
			}
			data, err := yaml.Marshal(cfg)
			if err != nil {
				return err
			}
			if _, err := cmd.OutOrStdout().Write(data); err != nil {
				return err
			}
			stillToDo(cmd, cfg)
			return nil
		},
	}
}

func pathCmd(path func() string) *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print where the config file is",
		Args:  cobra.NoArgs,
		Run:   func(cmd *cobra.Command, _ []string) { fmt.Fprintln(cmd.OutOrStdout(), path()) },
	}
}

const pathHelp = "A key is a dotted path through the config as `config show` prints it. A list item is picked by its\n" +
	"name (projects.web, sources.linear), a route by its project (sources.linear.routes.web), or by its index\n" +
	"from 0; + appends to a list (settings.labels.skip.+)."

func getCmd(path func() string) *cobra.Command {
	return &cobra.Command{
		Use:   "get <key>",
		Short: "Print one value, e.g. settings.maxTurns",
		Long:  "Prints one value.\n\n" + pathHelp,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := load(path())
			if err != nil {
				return err
			}
			root, err := toNode(cfg)
			if err != nil {
				return err
			}
			n, err := lookup(root, split(args[0]))
			if err != nil {
				return err
			}
			if n.Kind == yaml.ScalarNode {
				fmt.Fprintln(cmd.OutOrStdout(), n.Value)
				return nil
			}
			data, err := yaml.Marshal(n)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(data)
			return err
		},
	}
}

func setCmd(path func() string) *cobra.Command {
	var str bool
	c := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set one value, e.g. settings.maxTurns 3 or projects.web.branch 'work/{key}'",
		Long: "Sets one value. The value is read as YAML, so lists ([a, b]) and maps ({labels: [web]}) work; --string\n" +
			"takes it as text. A connection's secret field stores a plain value in the keyring.\n\n" + pathHelp,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := load(path())
			if err != nil {
				return err
			}
			segs := split(args[0])
			value := args[1]
			if ref, ok, err := secretValue(cfg, segs, value); err != nil {
				return err
			} else if ok {
				value, str = ref, true
			}
			v, err := parseValue(value, str)
			if err != nil {
				return err
			}
			root, err := toNode(cfg)
			if err != nil {
				return err
			}
			if err := assign(root, segs, v); err != nil {
				return err
			}
			// Decoded strictly, so a key the config doesn't have is an error, not a value quietly dropped.
			if err := known(root); err != nil {
				if v.Kind == yaml.ScalarNode && (v.Value == "" || v.Tag == "!!null") {
					return fmt.Errorf("to clear %s, use `%s config unset %s`", args[0], config.Name, args[0])
				}
				return fmt.Errorf("the config has no %s (see `%s config show` for the keys): %v", args[0], config.Name, err)
			}
			next, err := fromNode(root, cfg.Path)
			if err != nil {
				return err
			}
			if err := storeSecrets(next); err != nil {
				return err
			}
			return save(cmd, next, "set "+args[0])
		},
	}
	c.Flags().BoolVar(&str, "string", false, "take the value as text, not YAML")
	return c
}

func unsetCmd(path func() string) *cobra.Command {
	return &cobra.Command{
		Use:   "unset <key>",
		Short: "Remove one value (a setting goes back to its default)",
		Long:  "Removes one value; a setting goes back to its default.\n\n" + pathHelp,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := load(path())
			if err != nil {
				return err
			}
			root, err := toNode(cfg)
			if err != nil {
				return err
			}
			if err := remove(root, split(args[0])); err != nil {
				return err
			}
			next, err := fromNode(root, cfg.Path)
			if err != nil {
				return err
			}
			return save(cmd, next, "unset "+args[0])
		},
	}
}

// secretValue stores a plain value set on a connection's secret field and returns the reference to set.
func secretValue(cfg *config.Config, segs []string, value string) (string, bool, error) {
	if len(segs) != 3 || segs[0] != "connections" {
		return "", false, nil
	}
	c, ok := cfg.Connections[segs[1]]
	if !ok {
		return "", false, nil
	}
	spec, ok := adapter.Lookup(c.Type)
	if !ok {
		return "", false, nil
	}
	for _, f := range spec.Fields {
		if f.Key == segs[2] && f.Type == adapter.Secret && !config.IsReference(value) {
			ref, err := config.StoreSecret(segs[1]+"."+f.Key, value)
			return ref, err == nil, err
		}
	}
	return "", false, nil
}

// storeSecrets moves any plain value in a connection's secret field (set as part of a map) to the keyring.
func storeSecrets(cfg *config.Config) error {
	for name, c := range cfg.Connections {
		spec, ok := adapter.Lookup(c.Type)
		if !ok {
			continue
		}
		for _, f := range spec.Fields {
			if v := c.Values[f.Key]; f.Type == adapter.Secret && v != "" && !config.IsReference(v) {
				ref, err := config.StoreSecret(name+"."+f.Key, v)
				if err != nil {
					return err
				}
				c.Values[f.Key] = ref
			}
		}
	}
	return nil
}

// ---- connections ----

func connectionCmd(path func() string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "connection",
		Aliases: []string{"connections", "conn"},
		Short:   "Add, change, test or remove a connection (an agent, code host, tracker, tunnel, telemetry)",
	}
	var typ string
	var sets []string
	var noTest bool
	set := &cobra.Command{
		Use:   "set <name> [--type <type>] [--set key=value]...",
		Short: "Add a connection, or change one's fields; then test it",
		Long: "Adds a connection (--type is one of `" + config.Name + " adapters`) or changes an existing one's fields, then\n" +
			"tests it. `" + config.Name + " adapters <type>` lists a type's fields. A secret field takes a reference\n" +
			"(env:LINEAR_API_KEY) or a value, which is stored in the keyring; `config secret` asks for one unseen.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := load(path())
			if err != nil {
				return err
			}
			name := args[0]
			if !forms.ValidName(name) {
				return errors.New("a connection's name is letters, digits, dot, dash and underscore")
			}
			d := forms.NewConnectionDraft(cfg, "", "")
			if _, ok := cfg.Connections[name]; ok {
				d = forms.NewConnectionDraft(cfg, name, "")
				if typ != "" && typ != d.Type {
					return fmt.Errorf("%s is a %s connection; remove it to make it a %s", name, d.Type, typ)
				}
			} else {
				if typ == "" {
					return errors.New("a new connection needs --type (see `" + config.Name + " adapters`)")
				}
				if _, ok := adapter.Lookup(typ); !ok {
					return fmt.Errorf("no adapter %q (see `%s adapters`)", typ, config.Name)
				}
				d.Type = typ
			}
			d.Name = name
			d.FieldGroups(cfg)
			v, err := values(sets)
			if err != nil {
				return err
			}
			if err := setFields(d.Fields, v, name); err != nil {
				return err
			}
			if _, err := d.Apply(cfg); err != nil {
				return err
			}
			if err := save(cmd, cfg, "saved connection "+name); err != nil {
				return err
			}
			if !noTest {
				test(cmd, cfg, name)
			}
			return nil
		},
	}
	set.Flags().StringVar(&typ, "type", "", "the adapter, for a new connection")
	set.Flags().StringArrayVar(&sets, "set", nil, "a field, key=value (repeatable)")
	set.Flags().BoolVar(&noTest, "no-test", false, "don't test it")
	testCmd := &cobra.Command{
		Use:   "test <name>",
		Short: "Check a connection works",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := load(path())
			if err != nil {
				return err
			}
			if _, ok := cfg.Connections[args[0]]; !ok {
				return fmt.Errorf("no connection called %s", args[0])
			}
			if !test(cmd, cfg, args[0]) {
				return errors.New(args[0] + " didn't work")
			}
			return nil
		},
	}
	rm := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a connection nothing uses",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := load(path())
			if err != nil {
				return err
			}
			if _, ok := cfg.Connections[args[0]]; !ok {
				return fmt.Errorf("no connection called %s", args[0])
			}
			if uses := forms.Uses(cfg, args[0]); len(uses) > 0 {
				return fmt.Errorf("%s is used by %s", args[0], strings.Join(uses, ", "))
			}
			delete(cfg.Connections, args[0])
			return save(cmd, cfg, "removed connection "+args[0])
		},
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List the connections and what uses each",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := load(path())
			if err != nil {
				return err
			}
			names := make([]string, 0, len(cfg.Connections))
			for n := range cfg.Connections {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				uses := strings.Join(forms.Uses(cfg, n), ", ")
				if uses == "" {
					uses = "nothing"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%-16s %-14s used by %s\n", n, cfg.Connections[n].Type, uses)
			}
			return nil
		},
	}
	cmd.AddCommand(set, testCmd, rm, list)
	return cmd
}

func test(cmd *cobra.Command, cfg *config.Config, name string) bool {
	who, err := forms.TestConnection(cfg, name)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "✗ %s: %v\n", name, err)
		return false
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✓ %s works: %s\n", name, who)
	return true
}

func secretCmd(path func() string) *cobra.Command {
	return &cobra.Command{
		Use:   "secret <connection> <field>",
		Short: "Store a connection's secret (a token) in the keyring, typed unseen or read from stdin",
		Long: "Asks for the secret on the terminal without showing it (or reads it from stdin when piped), stores it in\n" +
			"the OS keyring (or the 0600 secrets file where there is none), and points the field at it.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := load(path())
			if err != nil {
				return err
			}
			c, ok := cfg.Connections[args[0]]
			if !ok {
				return fmt.Errorf("no connection called %s", args[0])
			}
			spec, _ := adapter.Lookup(c.Type)
			var field *adapter.Field
			for i, f := range spec.Fields {
				if f.Key == args[1] {
					field = &spec.Fields[i]
				}
			}
			if field == nil {
				return fmt.Errorf("%s has no field %q", args[0], args[1])
			}
			value, err := readSecret(cmd, fmt.Sprintf("%s for %s: ", field.Label, args[0]))
			if err != nil {
				return err
			}
			if value == "" {
				return errors.New("nothing entered")
			}
			ref := value
			if !config.IsReference(value) {
				if ref, err = config.StoreSecret(args[0]+"."+field.Key, value); err != nil {
					return err
				}
			}
			if c.Values == nil {
				c.Values = adapter.Values{}
			}
			c.Values[field.Key] = ref
			if err := save(cmd, cfg, fmt.Sprintf("stored %s's %s as %s", args[0], field.Key, ref)); err != nil {
				return err
			}
			test(cmd, cfg, args[0])
			return nil
		},
	}
}

func readSecret(cmd *cobra.Command, prompt string) (string, error) {
	in := cmd.InOrStdin()
	if f, ok := in.(*os.File); ok && term.IsTerminal(f.Fd()) {
		fmt.Fprint(cmd.ErrOrStderr(), prompt)
		b, err := term.ReadPassword(f.Fd())
		fmt.Fprintln(cmd.ErrOrStderr())
		return strings.TrimSpace(string(b)), err
	}
	b, err := io.ReadAll(in)
	return strings.TrimSpace(string(b)), err
}

// ---- projects ----

func projectCmd(path func() string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "project",
		Aliases: []string{"projects"},
		Short:   "Add, change or remove a project (a checkout the supervisor works issues in)",
	}
	var root, code, issues, source, agent, branch, base, setup, copyFiles, setupWhen string
	var repoSets, trackerSets []string
	var disabled, noSweep, noTriage, openIssues, landPR bool
	set := &cobra.Command{
		Use:   "set <name> --root <checkout> --code <connection> (--issues <connection> | --source <source>)",
		Short: "Add a project, or change one",
		Long: "Adds a project, or changes the flags given on an existing one. The repo binding is detected from the\n" +
			"checkout's origin; --repo and --tracker set binding fields (see `" + config.Name + " adapters <type>`).\n" +
			"--issues gives the project a tracker of its own; --source feeds it from a shared one, with a route\n" +
			"added by `config route set`.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := load(path())
			if err != nil {
				return err
			}
			name := args[0]
			if !forms.ValidName(name) {
				return errors.New("a project's name is letters, digits, dot, dash and underscore")
			}
			existing := cfg.Project(name)
			var d *forms.ProjectDraft
			if existing != nil {
				d = forms.NewProjectDraft(cfg, existing, "")
			} else {
				if root == "" {
					return errors.New("a new project needs --root, its git checkout")
				}
				d = forms.NewProjectDraft(cfg, nil, config.ExpandHome(root))
				d.Name = name
			}
			f := cmd.Flags()
			if f.Changed("root") {
				d.Root = root
			}
			if f.Changed("code") {
				d.Repo = code
			}
			if f.Changed("issues") && f.Changed("source") {
				return errors.New("--issues or --source, not both")
			}
			if f.Changed("issues") {
				d.Tracker = issues
			}
			if f.Changed("source") {
				d.Tracker = forms.FromSource + source
			}
			for flag, ptr := range map[string]*string{"agent": &d.Agent, "branch": &d.Branch, "base": &d.Base, "setup": &d.Setup, "copy": &d.Copy, "setup-when": &d.SetupWhen} {
				if f.Changed(flag) {
					v, _ := f.GetString(flag)
					*ptr = v
				}
			}
			if f.Changed("disabled") {
				d.Disabled = disabled
			}
			if f.Changed("no-sweep") {
				d.NoSweep = noSweep
			}
			if f.Changed("no-triage") {
				d.NoTriage = noTriage
			}
			if f.Changed("triage-issues") {
				d.OpenIssues = openIssues
			}
			if f.Changed("land-pr") {
				d.LandPR = landPR
			}
			if _, ok := cfg.Connections[d.Repo]; !ok {
				return fmt.Errorf("no code host connection %q (add one with `config connection set`, and pass --code)", d.Repo)
			}
			if _, ok := cfg.Connections[d.Tracker]; !ok && !strings.HasPrefix(d.Tracker, forms.FromSource) {
				return fmt.Errorf("no tracker connection %q (add one with `config connection set`, and pass --issues or --source)", d.Tracker)
			}
			d.BindingGroups(cfg)
			rv, err := values(repoSets)
			if err != nil {
				return err
			}
			tv, err := values(trackerSets)
			if err != nil {
				return err
			}
			if len(rv) > 0 {
				if d.RepoFields == nil {
					return errors.New("--repo: the code host has no repo fields")
				}
				if err := setFields(d.RepoFields, rv, "the repo binding"); err != nil {
					return err
				}
			}
			if len(tv) > 0 {
				if d.TrackerFields == nil {
					return errors.New("--tracker: the project has no tracker of its own with fields (a shared one's are on its source)")
				}
				if err := setFields(d.TrackerFields, tv, "the tracker binding"); err != nil {
					return err
				}
			}
			if d.RepoFields != nil {
				if err := d.RepoFields.Validate(); err != nil {
					return fmt.Errorf("repo: %w (set it with --repo)", err)
				}
			}
			p, err := d.Apply(cfg)
			if err != nil {
				return err
			}
			follow := d.Follow(cfg, p)
			if err := save(cmd, cfg, "saved project "+p.Name); err != nil {
				return err
			}
			if follow != nil {
				out := cmd.ErrOrStderr()
				if follow.Offer == "" {
					fmt.Fprintf(out, "next: send it issues with `%s config route set %s %s --when <property>=<values>`\n", config.Name, follow.Name, p.Name)
				} else {
					fmt.Fprintln(out, "warning: "+follow.Offer)
					if follow.Original == "" {
						var sets []string
						for _, kv := range forms.BindingArgs(forms.OwnTracker(p)) {
							sets = append(sets, "--set "+kv)
						}
						fmt.Fprintf(out, "  to share: `%s config source set %s --tracker %s %s`, then `config route set %s <project> --when ...` for %s\n",
							config.Name, follow.Name, follow.Tracker, strings.Join(sets, " "), follow.Name, strings.Join(follow.Projects, " and "))
					} else {
						fmt.Fprintf(out, "  to share: `%s config route set %s %s --when <property>=<values>`\n", config.Name, follow.Name, p.Name)
					}
				}
			}
			return nil
		},
	}
	f := set.Flags()
	f.StringVar(&root, "root", "", "the project's git checkout")
	f.StringVar(&code, "code", "", "the code host connection")
	f.StringVar(&issues, "issues", "", "a tracker connection of its own")
	f.StringVar(&source, "source", "", "a shared tracker (source) feeding it")
	f.StringVar(&agent, "agent", "", "the agent connection, if not the default")
	f.StringVar(&branch, "branch", "", "the branch template, with {key}")
	f.StringVar(&base, "base", "", "the base branch (blank asks the code host)")
	f.StringVar(&setup, "setup", "", "a command run in each new worktree")
	f.StringVar(&setupWhen, "setup-when", "", "comma-separated files that rerun setup when they change")
	f.StringVar(&copyFiles, "copy", "", "comma-separated files copied into each worktree, e.g. .env")
	f.StringArrayVar(&repoSets, "repo", nil, "a repo binding field, key=value (repeatable)")
	f.StringArrayVar(&trackerSets, "tracker", nil, "a tracker binding field, key=value (repeatable)")
	f.BoolVar(&disabled, "disabled", false, "leave the project off")
	f.BoolVar(&noSweep, "no-sweep", false, "turn off its daily corrections sweep")
	f.BoolVar(&noTriage, "no-triage", false, "turn off its daily triage of the problems agents report")
	f.BoolVar(&landPR, "land-pr", false, "let the sweep and triage open PRs instead of pushing to the main branch")
	f.BoolVar(&openIssues, "triage-issues", false, "let the triage open an issue per cause in its tracker, not only a report")
	rm := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a project and the routes to it (its state stays on disk)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := load(path())
			if err != nil {
				return err
			}
			if cfg.Project(args[0]) == nil {
				return fmt.Errorf("no project called %s", args[0])
			}
			forms.RemoveProject(cfg, args[0])
			return save(cmd, cfg, "removed project "+args[0])
		},
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List the projects and where each gets its issues",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := load(path())
			if err != nil {
				return err
			}
			for _, p := range cfg.Projects {
				code, from := "", "no issues"
				if p.Repo != nil {
					code = p.Repo.Connection
				}
				if src := cfg.SourceFor(p); src != nil && src.Implicit() {
					from = src.Tracker.Connection
				} else if src != nil {
					from = "via " + src.Name
				}
				off := ""
				if p.Disabled {
					off = " (off)"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%-16s %-36s code %-12s issues %s%s\n", p.Name, p.Root, code, from, off)
			}
			return nil
		},
	}
	cmd.AddCommand(set, rm, list)
	return cmd
}

// ---- sources and routes ----

func sourceCmd(path func() string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "source",
		Aliases: []string{"sources"},
		Short:   "Share one tracker between projects: add, change, inspect or remove a source",
	}
	var tracker, unrouted string
	var sets []string
	set := &cobra.Command{
		Use:   "set <name> --tracker <connection> [--set key=value]... [--unrouted ignore|label]",
		Short: "Add a shared tracker, or change one; routes are added with `config route set`",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := load(path())
			if err != nil {
				return err
			}
			name := args[0]
			var src *config.Source
			for _, s := range cfg.SourceList {
				if s.Name == name {
					src = s
				}
			}
			if src == nil {
				if cfg.Project(name) != nil {
					return fmt.Errorf("a project is called %s; a source needs a name of its own", name)
				}
				if !forms.ValidName(name) {
					return errors.New("a source's name is letters, digits, dot, dash and underscore")
				}
				if tracker == "" {
					return errors.New("a new source needs --tracker")
				}
				src = &config.Source{Name: name, Tracker: &config.Binding{Values: adapter.Values{}}}
				cfg.SourceList = append(cfg.SourceList, src)
			}
			if src.Tracker == nil {
				if tracker == "" {
					return fmt.Errorf("%s has no tracker: give it --tracker", name)
				}
				src.Tracker = &config.Binding{Values: adapter.Values{}}
			}
			if tracker != "" {
				c, ok := cfg.Connections[tracker]
				if !ok {
					return fmt.Errorf("no connection called %s", tracker)
				}
				if spec, ok := adapter.Lookup(c.Type); !ok || !spec.Has(adapter.RoleTracker) {
					return fmt.Errorf("%s can't hold issues", tracker)
				}
				if src.Tracker.Connection != tracker {
					src.Tracker = &config.Binding{Connection: tracker, Values: adapter.Values{}}
				}
			}
			v, err := values(sets)
			if err != nil {
				return err
			}
			c, ok := cfg.Connections[src.Tracker.Connection]
			if !ok {
				return fmt.Errorf("%s's tracker %s isn't a connection: give it --tracker", name, src.Tracker.Connection)
			}
			spec, _ := adapter.Lookup(c.Type)
			fs := forms.New(src.Tracker.Connection, spec.Binding[adapter.RoleTracker], src.Tracker.Values)
			if err := setFields(fs, v, "the tracker binding"); err != nil {
				return err
			}
			if src.Tracker.Values, err = fs.Apply(); err != nil {
				return err
			}
			switch unrouted {
			case "":
			case config.UnroutedIgnore:
				src.Unrouted = ""
			case config.UnroutedLabel:
				src.Unrouted = unrouted
			default:
				return errors.New("--unrouted is ignore or label")
			}
			done := "saved source " + name
			if len(src.Routes) == 0 {
				done += "; add its routes with `" + config.Name + " config route set " + name + " <project> --when <property>=<values>`"
			}
			return save(cmd, cfg, done)
		},
	}
	set.Flags().StringVar(&tracker, "tracker", "", "the tracker connection")
	set.Flags().StringArrayVar(&sets, "set", nil, "a tracker binding field, key=value (repeatable), e.g. team=ENG")
	set.Flags().StringVar(&unrouted, "unrouted", "", "what happens to issues no route takes: ignore, or label them")
	var itracker string
	var isets []string
	inspect := &cobra.Command{
		Use:   "inspect [<source or project>] [--tracker <connection> --set key=value...]",
		Short: "Read a tracker's open issues: the properties routes can test, and where the routes send them",
		Long: "Reads the open issues of a source (or a project's own tracker, or --tracker with its binding) and prints\n" +
			"each property they carry with its most common values, then how many issues each route takes.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := load(path())
			if err != nil {
				return err
			}
			var d *forms.SourceDraft
			switch {
			case len(args) == 1:
				src := cfg.FindSource(args[0])
				if src == nil {
					return fmt.Errorf("no source or project with a tracker of its own called %s", args[0])
				}
				if src.Implicit() {
					d = forms.NewSourceDraft(cfg, nil)
					d.SetTracker(src.Tracker.Connection, forms.OwnTracker(cfg.Project(args[0])))
				} else {
					// Its routes too, to show where the issues go.
					d = forms.NewSourceDraft(cfg, src)
				}
			case itracker != "":
				if _, ok := cfg.Connections[itracker]; !ok {
					return fmt.Errorf("no connection called %s", itracker)
				}
				v, err := values(isets)
				if err != nil {
					return err
				}
				d = forms.NewSourceDraft(cfg, nil)
				d.SetTracker(itracker, v)
			default:
				return errors.New("name a source, or give --tracker")
			}
			text, err := d.Inspect(cfg)
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), text)
			return nil
		},
	}
	inspect.Flags().StringVar(&itracker, "tracker", "", "a tracker connection to read, without a source")
	inspect.Flags().StringArrayVar(&isets, "set", nil, "its binding, key=value (repeatable)")
	rm := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a source; each project it fed reads the tracker on its own again",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := load(path())
			if err != nil {
				return err
			}
			found := false
			for _, s := range cfg.SourceList {
				found = found || s.Name == args[0]
			}
			if !found {
				return fmt.Errorf("no source called %s", args[0])
			}
			forms.RemoveSource(cfg, args[0])
			return save(cmd, cfg, "removed source "+args[0])
		},
	}
	cmd.AddCommand(set, inspect, rm)
	return cmd
}

func routeCmd(path func() string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "route",
		Aliases: []string{"routes"},
		Short:   "Decide which project takes each of a source's issues",
		Long: "A source's routes are tried in order and the first that matches takes the issue. A route with no --when\n" +
			"takes everything the others don't, and goes last.",
	}
	var when []string
	var match string
	var position int
	var also bool
	set := &cobra.Command{
		Use:   "set <source> <project> [--when property=value,value]... [--match <yaml>]",
		Short: "Send the issues that match to a project (replacing the source's routes to it)",
		Long: "Sends a source's issues that match to the project, replacing the source's routes to it (--also adds\n" +
			"one). Each --when is a property and the values it may have, comma-separated, * and ? as wildcards, case\n" +
			"ignored; several --when must all match. --match takes a full match as YAML for any/all/not, e.g.\n" +
			"'{labels: [web], not: {state: [Triage]}}'. `config source inspect <source>` lists the properties.\n" +
			"The project stops reading a tracker of its own.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := load(path())
			if err != nil {
				return err
			}
			m := &config.Match{}
			if match != "" {
				if err := yaml.Unmarshal([]byte(match), m); err != nil {
					return fmt.Errorf("--match: %w", err)
				}
			}
			for _, w := range when {
				k, v, ok := strings.Cut(w, "=")
				k = strings.TrimSpace(k)
				if !ok || k == "" {
					return fmt.Errorf("--when %q: want property=value,value", w)
				}
				var vals []string
				for _, x := range strings.Split(v, ",") {
					if x = strings.TrimSpace(x); x != "" {
						vals = append(vals, x)
					}
				}
				if len(vals) == 0 {
					return fmt.Errorf("--when %q: no values", w)
				}
				if _, taken := m.Fields[k]; taken {
					// The same property again must match too, not widen the first.
					m.All = append(m.All, &config.Match{Fields: map[string][]string{k: vals}})
					continue
				}
				if m.Fields == nil {
					m.Fields = map[string][]string{}
				}
				m.Fields[k] = vals
			}
			note, err := forms.SetRoute(cfg, args[0], args[1], m, position, also)
			if err != nil {
				return err
			}
			done := fmt.Sprintf("%s sends %s to %s", args[0], describe(m), args[1])
			if note != "" {
				done += " (" + note + ")"
			}
			return save(cmd, cfg, done)
		},
	}
	set.Flags().StringArrayVar(&when, "when", nil, "property=value,value (repeatable; all must match)")
	set.Flags().StringVar(&match, "match", "", "a match as YAML, for any/all/not")
	set.Flags().IntVar(&position, "position", 0, "where the route goes, from 1 (default: its old place, else before the catch-all)")
	set.Flags().BoolVar(&also, "also", false, "add a route rather than replacing the project's")
	rm := &cobra.Command{
		Use:   "remove <source> <project>",
		Short: "Take a source's routes to a project out",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := load(path())
			if err != nil {
				return err
			}
			note, err := forms.RemoveRoutes(cfg, args[0], args[1])
			if err != nil {
				return err
			}
			done := "removed " + args[0] + "'s routes to " + args[1]
			if note != "" {
				done += " (" + note + ")"
			}
			return save(cmd, cfg, done)
		},
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List every source's routes, in order",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := load(path())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, s := range cfg.Sources() {
				conn := ""
				if s.Tracker != nil {
					conn = s.Tracker.Connection
				}
				kind := "shared"
				if s.Implicit() {
					kind = "its own"
				}
				fmt.Fprintf(out, "%s (%s, %s)\n", s.Name, conn, kind)
				for i, r := range s.Routes {
					fmt.Fprintf(out, "  %d. %-16s %s\n", i+1, r.Project, describe(r.When))
				}
			}
			return nil
		},
	}
	cmd.AddCommand(set, rm, list)
	return cmd
}

func describe(m *config.Match) string {
	if m.Empty() {
		return "everything else"
	}
	return "issues with " + m.String()
}

// ---- paths through the config's YAML ----

// known checks the YAML decodes into a config with no field left over.
func known(n *yaml.Node) error {
	data, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	return dec.Decode(config.New(""))
}

func split(key string) []string { return strings.Split(strings.Trim(key, "."), ".") }

func toNode(cfg *config.Config) (*yaml.Node, error) {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	return doc.Content[0], nil
}

func fromNode(n *yaml.Node, path string) (*config.Config, error) {
	data, err := yaml.Marshal(n)
	if err != nil {
		return nil, err
	}
	return config.Parse(path, data)
}

func parseValue(s string, str bool) (*yaml.Node, error) {
	if !str {
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(s), &doc); err == nil && len(doc.Content) == 1 {
			return doc.Content[0], nil
		}
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}, nil
}

// child finds a mapping's value by key, or a list's item by index, name or project; -1 when there's none.
func child(n *yaml.Node, seg string) int {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == seg {
				return i + 1
			}
		}
	case yaml.SequenceNode:
		if i, err := strconv.Atoi(seg); err == nil {
			if i >= 0 && i < len(n.Content) {
				return i
			}
			return -1
		}
		for i, item := range n.Content {
			if item.Kind != yaml.MappingNode {
				continue
			}
			for _, key := range []string{"name", "project"} {
				if j := child(item, key); j >= 0 && item.Content[j].Value == seg {
					return i
				}
			}
		}
	}
	return -1
}

func lookup(n *yaml.Node, segs []string) (*yaml.Node, error) {
	for i, seg := range segs {
		j := child(n, seg)
		if j < 0 {
			return nil, fmt.Errorf("nothing at %s", strings.Join(segs[:i+1], "."))
		}
		n = n.Content[j]
	}
	return n, nil
}

// assign sets the value at the path, making the maps on the way.
func assign(n *yaml.Node, segs []string, v *yaml.Node) error {
	for i, seg := range segs {
		last := i == len(segs)-1
		if n.Kind == yaml.SequenceNode && seg == "+" {
			if !last {
				return errors.New("+ appends, so it comes last")
			}
			n.Content = append(n.Content, v)
			return nil
		}
		j := child(n, seg)
		if j < 0 {
			if n.Kind != yaml.MappingNode {
				if n.Kind == yaml.ScalarNode && n.Tag == "!!null" {
					n.Kind, n.Tag, n.Value = yaml.MappingNode, "", ""
				} else {
					return fmt.Errorf("nothing at %s: add list items with the config commands (connection, project, source, route) or +", strings.Join(segs[:i+1], "."))
				}
			}
			next := v
			if !last {
				next = &yaml.Node{Kind: yaml.MappingNode}
				if segs[i+1] == "+" {
					next = &yaml.Node{Kind: yaml.SequenceNode}
				}
			}
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: seg}, next)
			n = next
			continue
		}
		if last {
			n.Content[j] = v
			return nil
		}
		n = n.Content[j]
	}
	return nil
}

func remove(n *yaml.Node, segs []string) error {
	parent, err := lookup(n, segs[:len(segs)-1])
	if err != nil {
		return err
	}
	j := child(parent, segs[len(segs)-1])
	if j < 0 {
		return fmt.Errorf("nothing at %s", strings.Join(segs, "."))
	}
	if parent.Kind == yaml.MappingNode {
		parent.Content = append(parent.Content[:j-1], parent.Content[j+1:]...)
	} else {
		parent.Content = append(parent.Content[:j], parent.Content[j+1:]...)
	}
	return nil
}
