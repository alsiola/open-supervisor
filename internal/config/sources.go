package config

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/alsiola/open-supervisor/internal/adapter"
)

// Source is one tracker feeding several projects: its issues are read once, its hook is registered once, and
// each issue goes to the first project whose route matches it. A project with a tracker of its own is a source
// of one, with a single route that takes everything (see Config.Sources).
type Source struct {
	Name    string   `yaml:"name"`
	Tracker *Binding `yaml:"tracker"`
	Routes  []Route  `yaml:"routes"`
	// Unrouted is what happens to an issue no route takes: "ignore" (the default) leaves it alone, "label"
	// puts the unrouted label on it so people can see it needs a home.
	Unrouted string `yaml:"unrouted,omitempty"`
	// Labels override the settings' labels for every project the source feeds: the claimed and skip labels
	// live on the shared tracker, so they belong to it rather than to any one project.
	Labels *Labels `yaml:"labels,omitempty"`

	// implicit: made from a project's own tracker, not written in the config.
	implicit bool
}

// Implicit reports whether the source is a project's own tracker rather than one in the config's sources.
func (s *Source) Implicit() bool { return s.implicit }

// Route sends the issues When matches to Project. An empty When takes everything.
type Route struct {
	Project string `yaml:"project"`
	When    *Match `yaml:"when,omitempty"`
}

// RouteFor is the index of the first route that takes an issue with these properties, -1 for none.
func (s *Source) RouteFor(attr func(name string) []string) int {
	for i, r := range s.Routes {
		if r.When.Matches(attr) {
			return i
		}
	}
	return -1
}

// Unrouted policies.
const (
	UnroutedIgnore = "ignore"
	UnroutedLabel  = "label"
)

// Match is a test on an issue's properties. Fields are ANDed; a field's patterns are ORed, and a pattern
// matches when any of the property's values does. Patterns ignore case and take * and ? as wildcards. Any
// matches when one of its matches does, All when every one does, Not when its match doesn't.
type Match struct {
	Fields map[string][]string
	Any    []*Match
	All    []*Match
	Not    *Match
}

// Matches tests the issue's properties, read through attr ("labels", "project", a custom field).
func (m *Match) Matches(attr func(name string) []string) bool {
	if m == nil {
		return true
	}
	for name, patterns := range m.Fields {
		if !anyMatch(patterns, attr(name)) {
			return false
		}
	}
	for _, x := range m.All {
		if !x.Matches(attr) {
			return false
		}
	}
	if len(m.Any) > 0 {
		ok := false
		for _, x := range m.Any {
			if x.Matches(attr) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if m.Not != nil && m.Not.Matches(attr) {
		return false
	}
	return true
}

// Empty reports whether the match tests nothing, so takes everything.
func (m *Match) Empty() bool {
	return m == nil || len(m.Fields) == 0 && len(m.Any) == 0 && len(m.All) == 0 && m.Not == nil
}

// String is the match written out for the log and `route`: labels=web,ios project="API v2".
func (m *Match) String() string {
	if m.Empty() {
		return "everything"
	}
	var parts []string
	names := make([]string, 0, len(m.Fields))
	for k := range m.Fields {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		vals := make([]string, len(m.Fields[k]))
		for i, v := range m.Fields[k] {
			if strings.ContainsAny(v, " ,") {
				v = fmt.Sprintf("%q", v)
			}
			vals[i] = v
		}
		parts = append(parts, k+"="+strings.Join(vals, ","))
	}
	group := func(word string, ms []*Match) {
		if len(ms) == 0 {
			return
		}
		inner := make([]string, len(ms))
		for i, x := range ms {
			inner[i] = "(" + x.String() + ")"
		}
		parts = append(parts, word+" "+strings.Join(inner, " "))
	}
	group("all", m.All)
	group("any", m.Any)
	if m.Not != nil {
		parts = append(parts, "not ("+m.Not.String()+")")
	}
	return strings.Join(parts, " ")
}

func anyMatch(patterns, values []string) bool {
	for _, p := range patterns {
		for _, v := range values {
			if Glob(p, v) {
				return true
			}
		}
	}
	return false
}

// Glob matches s against a pattern with * (any run, slashes included) and ? (one character), ignoring case.
func Glob(pattern, s string) bool {
	p, x := []rune(strings.ToLower(pattern)), []rune(strings.ToLower(s))
	// The classic two-pointer match with backtracking to the last star.
	pi, xi, star, mark := 0, 0, -1, 0
	for xi < len(x) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == x[xi]):
			pi++
			xi++
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, xi
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			xi = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// UnmarshalYAML reads a match: a map of property names to a value or a list of values, with any, all and not
// as the combinators.
func (m *Match) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: a match is a map of properties to values", n.Line)
	}
	*m = Match{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i].Value, n.Content[i+1]
		switch key {
		case "any", "all":
			var list []*Match
			if err := val.Decode(&list); err != nil {
				return err
			}
			if len(list) == 0 {
				return fmt.Errorf("line %d: %s needs at least one match", val.Line, key)
			}
			if key == "any" {
				m.Any = list
			} else {
				m.All = list
			}
		case "not":
			var x Match
			if err := val.Decode(&x); err != nil {
				return err
			}
			m.Not = &x
		default:
			var vals []string
			switch val.Kind {
			case yaml.ScalarNode:
				vals = []string{val.Value}
			case yaml.SequenceNode:
				for _, c := range val.Content {
					if c.Kind != yaml.ScalarNode {
						return fmt.Errorf("line %d: %s takes values, not maps", c.Line, key)
					}
					vals = append(vals, c.Value)
				}
			default:
				return fmt.Errorf("line %d: %s takes a value or a list of values", val.Line, key)
			}
			if len(vals) == 0 {
				return fmt.Errorf("line %d: %s needs at least one value", val.Line, key)
			}
			if m.Fields == nil {
				m.Fields = map[string][]string{}
			}
			m.Fields[key] = vals
		}
	}
	return nil
}

// MarshalYAML writes a match back the way it is read: one value as a scalar, more as a list.
func (m *Match) MarshalYAML() (any, error) {
	out := &yaml.Node{Kind: yaml.MappingNode}
	add := func(key string, v any) error {
		var val yaml.Node
		if err := val.Encode(v); err != nil {
			return err
		}
		out.Content = append(out.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, &val)
		return nil
	}
	names := make([]string, 0, len(m.Fields))
	for k := range m.Fields {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		var v any = m.Fields[k]
		if len(m.Fields[k]) == 1 {
			v = m.Fields[k][0]
		}
		if err := add(k, v); err != nil {
			return nil, err
		}
	}
	if len(m.All) > 0 {
		if err := add("all", m.All); err != nil {
			return nil, err
		}
	}
	if len(m.Any) > 0 {
		if err := add("any", m.Any); err != nil {
			return nil, err
		}
	}
	if m.Not != nil {
		if err := add("not", m.Not); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Sources are every source the enabled projects get issues from: the config's own, then one for each project
// with a tracker of its own, named after it.
func (c *Config) Sources() []*Source {
	var out []*Source
	out = append(out, c.SourceList...)
	for _, p := range c.Projects {
		if p.Tracker != nil && !p.Disabled {
			out = append(out, implicitSource(c, p))
		}
	}
	return out
}

func implicitSource(c *Config, p *Project) *Source {
	return &Source{Name: p.Name, Tracker: p.Tracker, Routes: []Route{{Project: p.Name}}, Labels: p.Labels, implicit: true}
}

// FindSource finds a source by name, a project's own included.
func (c *Config) FindSource(name string) *Source {
	for _, s := range c.SourceList {
		if s.Name == name {
			return s
		}
	}
	if p := c.Project(name); p != nil && p.Tracker != nil {
		return implicitSource(c, p)
	}
	return nil
}

// SourceFor is the source a project gets its issues from: its own tracker, or the source with a route to it.
func (c *Config) SourceFor(p *Project) *Source {
	if p.Tracker != nil {
		return implicitSource(c, p)
	}
	for _, s := range c.SourceList {
		for _, r := range s.Routes {
			if r.Project == p.Name {
				return s
			}
		}
	}
	return nil
}

// SourceLabels are a source's labels.
func (c *Config) SourceLabels(s *Source) Labels {
	if s.Labels == nil {
		return c.Settings.Labels
	}
	return s.Labels.Merge(c.Settings.Labels)
}

// validateSources checks the sources and that every project gets issues from exactly one place.
func (c *Config) validateSources(role func(where, name string, r adapter.Role), errs *[]string) {
	add := func(format string, args ...any) { *errs = append(*errs, fmt.Sprintf(format, args...)) }
	names := map[string]bool{}
	fedBy := map[string]string{}
	for i, s := range c.SourceList {
		where := fmt.Sprintf("source %d", i+1)
		if s.Name != "" {
			where = "source " + s.Name
		}
		if !nameRe.MatchString(s.Name) {
			add("%s: needs a name of letters, digits, dot, dash and underscore", where)
		}
		if names[s.Name] {
			add("%s: named twice", where)
		}
		names[s.Name] = true
		if c.Project(s.Name) != nil {
			add("%s: a project has the same name; sources and projects share the webhook paths", where)
		}
		if s.Tracker == nil || s.Tracker.Connection == "" {
			add("%s: needs a tracker connection", where)
		} else {
			role(where+" tracker", s.Tracker.Connection, adapter.RoleTracker)
		}
		switch s.Unrouted {
		case "", UnroutedIgnore, UnroutedLabel:
		default:
			add("%s: unrouted is %q or %q, not %q", where, UnroutedIgnore, UnroutedLabel, s.Unrouted)
		}
		if len(s.Routes) == 0 {
			add("%s: has no routes", where)
		}
		for j, r := range s.Routes {
			p := c.Project(r.Project)
			switch {
			case r.Project == "":
				add("%s: route %d names no project", where, j+1)
			case p == nil:
				add("%s: route %d: no project named %q", where, j+1, r.Project)
			case p.Tracker != nil:
				add("%s: route %d: project %s has a tracker of its own; take it out to feed the project from %s", where, j+1, r.Project, s.Name)
			case fedBy[r.Project] != "" && fedBy[r.Project] != s.Name:
				add("project %s: routed from both %s and %s; a project takes issues from one source", r.Project, fedBy[r.Project], s.Name)
			default:
				fedBy[r.Project] = s.Name
				if p.Labels != nil {
					add("project %s: its labels belong to its source; set them on %s", r.Project, s.Name)
				}
			}
			if r.When.Empty() && j < len(s.Routes)-1 {
				add("%s: route %d takes everything, so the routes after it never match", where, j+1)
			}
		}
	}
	for _, p := range c.Projects {
		if p.Tracker == nil && fedBy[p.Name] == "" && p.Name != "" {
			add("project %s: needs a tracker connection, or a source with a route to it", p.Name)
		}
	}
}

// Clone is a deep copy of the source.
func (s *Source) Clone() *Source {
	cp := *s
	if s.Tracker != nil {
		v := adapter.Values{}
		for k, x := range s.Tracker.Values {
			v[k] = x
		}
		cp.Tracker = &Binding{Connection: s.Tracker.Connection, Values: v}
	}
	cp.Routes = make([]Route, len(s.Routes))
	for i, r := range s.Routes {
		cp.Routes[i] = Route{Project: r.Project, When: r.When.Clone()}
	}
	if s.Labels != nil {
		l := *s.Labels
		l.Skip = append([]string(nil), s.Labels.Skip...)
		cp.Labels = &l
	}
	return &cp
}

// Clone is a deep copy of the match.
func (m *Match) Clone() *Match {
	if m == nil {
		return nil
	}
	cp := &Match{Not: m.Not.Clone()}
	if m.Fields != nil {
		cp.Fields = map[string][]string{}
		for k, v := range m.Fields {
			cp.Fields[k] = append([]string(nil), v...)
		}
	}
	for _, x := range m.Any {
		cp.Any = append(cp.Any, x.Clone())
	}
	for _, x := range m.All {
		cp.All = append(cp.All, x.Clone())
	}
	return cp
}
