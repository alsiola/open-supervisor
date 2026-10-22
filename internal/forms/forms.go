// Package forms builds huh forms from the fields an adapter declares, so the onboarding wizard and the TUI's
// config editor share one way of asking for a connection's or a binding's settings. A field that only applies
// when another has a value (an auth mode's own fields) sits in a group that hides itself otherwise.
package forms

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/config"
	"github.com/alsiola/open-supervisor/internal/engine"
)

// keptNote is shown on a secret that already holds a reference.
const keptNote = "(stored; leave blank to keep)"

// FieldSet is a set of adapter fields being edited: each field's live value, bound to the huh fields built
// from it, so hide functions and Apply see what has been typed.
type FieldSet struct {
	Fields []adapter.Field
	// Name is the connection the values belong to, for naming stored secrets.
	Name   string
	strs   map[string]*string
	bools  map[string]*bool
	before adapter.Values
}

// New prepares fields for editing, starting from values (the field defaults when a value is missing).
func New(name string, fields []adapter.Field, values adapter.Values) *FieldSet {
	fs := &FieldSet{Fields: fields, Name: name, strs: map[string]*string{}, bools: map[string]*bool{}, before: adapter.Values{}}
	for k, v := range values {
		fs.before[k] = v
	}
	for _, f := range fields {
		v, ok := values[f.Key]
		if !ok || v == "" {
			v = f.Default
		}
		switch f.Type {
		case adapter.Bool:
			b, _ := strconv.ParseBool(v)
			fs.bools[f.Key] = &b
		case adapter.Secret:
			// A stored secret is never shown; blank keeps it.
			s := ""
			if v != "" && !config.IsReference(v) {
				s = v
			}
			fs.strs[f.Key] = &s
		default:
			s := v
			fs.strs[f.Key] = &s
		}
	}
	return fs
}

// Current is the values as typed so far, for ShowIf.
func (fs *FieldSet) Current() adapter.Values {
	v := adapter.Values{}
	for k, s := range fs.strs {
		v[k] = *s
	}
	for k, b := range fs.bools {
		v[k] = strconv.FormatBool(*b)
	}
	return v
}

// Visible reports whether a field applies given what has been typed.
func (fs *FieldSet) Visible(f adapter.Field) bool { return f.Visible(fs.Current()) }

func showIfKey(f adapter.Field) string {
	if len(f.ShowIf) == 0 {
		return ""
	}
	parts := []string{}
	for k, vs := range f.ShowIf {
		parts = append(parts, k+"="+strings.Join(vs, "|"))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// Groups are huh groups for the fields, in order. Consecutive fields with the same ShowIf share a group, which
// hides itself when the condition doesn't hold.
func (fs *FieldSet) Groups(title string) []*huh.Group {
	var groups []*huh.Group
	var cur []huh.Field
	var curKey string
	var curField *adapter.Field
	flush := func() {
		if len(cur) == 0 {
			return
		}
		g := huh.NewGroup(cur...)
		if len(groups) == 0 && title != "" {
			g = g.Title(title)
		}
		if curField != nil && len(curField.ShowIf) > 0 {
			f := *curField
			g = g.WithHideFunc(func() bool { return !fs.Visible(f) })
		}
		groups = append(groups, g)
		cur = nil
	}
	for i := range fs.Fields {
		f := fs.Fields[i]
		k := showIfKey(f)
		if k != curKey || curField == nil {
			flush()
			curKey = k
			curField = &fs.Fields[i]
		}
		cur = append(cur, fs.field(f))
	}
	flush()
	return groups
}

func label(f adapter.Field) string {
	l := f.Label
	if l == "" {
		l = f.Key
	}
	if f.Required {
		l += " *"
	}
	return l
}

func (fs *FieldSet) field(f adapter.Field) huh.Field {
	switch f.Type {
	case adapter.Bool:
		return huh.NewConfirm().Title(label(f)).Description(f.Help).Value(fs.bools[f.Key])
	case adapter.Select:
		opts := make([]huh.Option[string], 0, len(f.Options))
		for _, o := range f.Options {
			opts = append(opts, huh.NewOption(o, o))
		}
		if len(opts) == 0 {
			opts = append(opts, huh.NewOption("(none)", ""))
		}
		return huh.NewSelect[string]().Title(label(f)).Description(f.Help).Options(opts...).Value(fs.strs[f.Key])
	}
	in := huh.NewInput().Title(label(f)).Description(f.Help).Value(fs.strs[f.Key])
	placeholder := f.Placeholder
	if f.Type == adapter.Secret {
		in = in.EchoMode(huh.EchoModePassword)
		if old := fs.before[f.Key]; old != "" && config.IsReference(old) {
			placeholder = keptNote
		}
	}
	if placeholder != "" {
		in = in.Placeholder(placeholder)
	}
	in = in.Validate(fs.validator(f))
	return in
}

func (fs *FieldSet) validator(f adapter.Field) func(string) error {
	return func(s string) error {
		s = strings.TrimSpace(s)
		if !fs.Visible(f) {
			return nil
		}
		if s == "" {
			if f.Required && !(f.Type == adapter.Secret && fs.before[f.Key] != "") {
				return fmt.Errorf("%s is required", f.Label)
			}
			return nil
		}
		if f.Type == adapter.Int {
			if _, err := strconv.Atoi(s); err != nil {
				return fmt.Errorf("%s must be a whole number", f.Label)
			}
		}
		return nil
	}
}

// Validate checks the typed values the way the huh validators do, for callers that skip the form.
func (fs *FieldSet) Validate() error {
	for _, f := range fs.Fields {
		if f.Type == adapter.Bool {
			continue
		}
		if err := fs.validator(f)(*fs.strs[f.Key]); err != nil {
			return err
		}
	}
	return nil
}

// Set puts a value in as though it had been typed (tests, defaults from detection).
func (fs *FieldSet) Set(key, value string) {
	if b, ok := fs.bools[key]; ok {
		*b, _ = strconv.ParseBool(value)
		return
	}
	if s, ok := fs.strs[key]; ok {
		*s = value
	}
}

// Apply returns the values to put in the config. Fields that don't apply are dropped; a secret typed in is
// stored (keyring, else the secrets file) and its reference kept; a blank secret keeps the one stored before; a
// secret typed as a reference (env:, cmd:, keyring:, file:) is kept as typed.
func (fs *FieldSet) Apply() (adapter.Values, error) {
	out := adapter.Values{}
	for k, v := range fs.before {
		if !fs.known(k) {
			out[k] = v // values no field covers are kept
		}
	}
	for _, f := range fs.Fields {
		if !fs.Visible(f) {
			continue
		}
		switch f.Type {
		case adapter.Bool:
			out[f.Key] = strconv.FormatBool(*fs.bools[f.Key])
		case adapter.Secret:
			v := strings.TrimSpace(*fs.strs[f.Key])
			switch {
			case v == "":
				if old := fs.before[f.Key]; old != "" {
					out[f.Key] = old
				}
			case config.IsReference(v):
				out[f.Key] = v
			default:
				name := fmt.Sprintf("%s.%s", fs.Name, f.Key)
				ref, err := config.StoreSecret(name, v)
				if err != nil {
					return nil, fmt.Errorf("storing %s: %w", f.Label, err)
				}
				out[f.Key] = ref
			}
		default:
			if v := strings.TrimSpace(*fs.strs[f.Key]); v != "" {
				out[f.Key] = v
			}
		}
	}
	return out, nil
}

func (fs *FieldSet) known(key string) bool {
	for _, f := range fs.Fields {
		if f.Key == key {
			return true
		}
	}
	return false
}

// TestConnection opens the named connection from cfg and checks it works, within 20 seconds.
func TestConnection(cfg *config.Config, name string) (string, error) {
	env := adapter.Env{
		Log:     func(string, ...any) {},
		Secret:  config.ResolveSecret,
		DataDir: config.DataDir(),
		HTTP:    &http.Client{Timeout: 20 * time.Second},
	}
	conn, err := engine.Open(cfg, name, env)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return conn.Check(ctx)
}

// Clone is a deep copy of a config, so an edit can be thrown away.
func Clone(c *config.Config) *config.Config {
	out := &config.Config{Version: c.Version, Settings: c.Settings, Path: c.Path, Connections: map[string]*config.Connection{}}
	out.Settings.Telemetry = append([]string(nil), c.Settings.Telemetry...)
	out.Settings.Labels.Skip = append([]string(nil), c.Settings.Labels.Skip...)
	out.Settings.Labels.Require = append([]string(nil), c.Settings.Labels.Require...)
	for k, v := range c.Connections {
		out.Connections[k] = &config.Connection{Type: v.Type, Values: copyValues(v.Values)}
	}
	for _, p := range c.Projects {
		out.Projects = append(out.Projects, CloneProject(p))
	}
	for _, s := range c.SourceList {
		out.SourceList = append(out.SourceList, s.Clone())
	}
	return out
}

// CloneProject is a deep copy of one project.
func CloneProject(p *config.Project) *config.Project {
	cp := *p
	if p.Repo != nil {
		cp.Repo = &config.Binding{Connection: p.Repo.Connection, Values: copyValues(p.Repo.Values)}
	}
	if p.Tracker != nil {
		cp.Tracker = &config.Binding{Connection: p.Tracker.Connection, Values: copyValues(p.Tracker.Values)}
	}
	if p.Labels != nil {
		l := *p.Labels
		l.Skip = append([]string(nil), p.Labels.Skip...)
		l.Require = append([]string(nil), p.Labels.Require...)
		cp.Labels = &l
	}
	cp.Worktree.Copy = append([]string(nil), p.Worktree.Copy...)
	cp.Worktree.SetupWhen = append([]string(nil), p.Worktree.SetupWhen...)
	cp.Sweep.RuleFiles = append([]string(nil), p.Sweep.RuleFiles...)
	cp.Jobs = append([]config.Job(nil), p.Jobs...)
	if p.Env != nil {
		cp.Env = map[string]string{}
		for k, v := range p.Env {
			cp.Env[k] = v
		}
	}
	return &cp
}

func copyValues(v adapter.Values) adapter.Values {
	out := adapter.Values{}
	for k, x := range v {
		out[k] = x
	}
	return out
}

// Theme is the huh theme both the wizard and the editor use.
func Theme() *huh.Theme { return huh.ThemeCharm() }

// KeyMap is huh's keys, with esc also cancelling, for forms inside the TUI.
func KeyMap() *huh.KeyMap {
	k := huh.NewDefaultKeyMap()
	k.Quit.SetKeys("ctrl+c", "esc")
	return k
}

// openQuiet opens a connection with no logging, for detection.
func openQuiet(cfg *config.Config, name string) (adapter.Connection, error) {
	return engine.Open(cfg, name, adapter.Env{
		Log:     func(string, ...any) {},
		Secret:  config.ResolveSecret,
		DataDir: config.DataDir(),
		HTTP:    &http.Client{Timeout: 20 * time.Second},
	})
}
