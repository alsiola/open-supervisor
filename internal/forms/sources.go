package forms

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/huh"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/config"
	"github.com/alsiola/open-supervisor/internal/core"
	"github.com/alsiola/open-supervisor/internal/engine"
)

// ---- sources --------------------------------------------------------------------------------------------

// otherProperty is the property choice for one typed in rather than picked from what the tracker showed.
const otherProperty = "@other"

// SourceDraft is a source being added or edited: one tracker shared by several projects, and the routes that
// send each of its issues to one of them. The form asks for a single property to split the issues by and, per
// project, the values that send an issue there; routes written by hand that test more than that (any, not,
// several properties) are kept as they are, and only projects new to the source are asked about.
type SourceDraft struct {
	Original string
	Name     string
	Tracker  string
	Unrouted string
	Projects []string // the projects the source feeds
	By       string   // the property the routes test
	Other    string   // the property, when typed in
	Values   map[string]*string

	// Offer, when set, is asked first, and the draft does nothing unless the answer is yes.
	Offer    string
	Accepted bool

	TrackerFields *FieldSet
	trackerValues adapter.Values
	advanced      bool
	src           *config.Source

	issues    []core.Issue
	loadErr   error
	loadedFor string
}

// NewSourceDraft starts a draft from one of the config's sources (nil for a new one).
func NewSourceDraft(cfg *config.Config, src *config.Source) *SourceDraft {
	d := &SourceDraft{Values: map[string]*string{}, Accepted: true, Unrouted: config.UnroutedIgnore, By: "labels"}
	if src == nil {
		if names := cfg.ConnectionNames(adapter.RoleTracker); len(names) > 0 {
			d.Tracker = names[0]
			d.Name = UniqueSourceName(cfg, names[0])
		}
		return d
	}
	d.src = src.Clone()
	d.Original, d.Name = src.Name, src.Name
	if src.Unrouted != "" {
		d.Unrouted = src.Unrouted
	}
	if src.Tracker != nil {
		d.Tracker = src.Tracker.Connection
		d.trackerValues = copyValues(src.Tracker.Values)
	}
	seen := map[string]bool{}
	for _, r := range src.Routes {
		if !seen[r.Project] {
			seen[r.Project] = true
			d.Projects = append(d.Projects, r.Project)
		}
	}
	d.advanced = len(seen) != len(src.Routes)
	d.By = ""
	defer func() {
		if d.By == "" {
			d.By = "labels"
		}
	}()
	for _, r := range src.Routes {
		v := ""
		switch {
		case r.When.Empty():
		case len(r.When.Fields) == 1 && len(r.When.Any) == 0 && len(r.When.All) == 0 && r.When.Not == nil:
			for k, pats := range r.When.Fields {
				if d.By != "" && !strings.EqualFold(d.By, k) {
					d.advanced = true
				}
				d.By = k
				v = strings.Join(pats, ", ")
			}
		default:
			d.advanced = true
		}
		d.Values[r.Project] = &v
	}
	return d
}

// UniqueSourceName is base, or base with a number, so that no source or project already has it.
func UniqueSourceName(cfg *config.Config, base string) string {
	name := base
	for i := 2; ; i++ {
		if cfg.Project(name) == nil && findSource(cfg, name) == nil {
			return name
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
}

func findSource(cfg *config.Config, name string) *config.Source {
	for _, s := range cfg.SourceList {
		if s.Name == name {
			return s
		}
	}
	return nil
}

// Advanced reports whether the source's routes test more than the form can write, so they are kept as they are.
func (d *SourceDraft) Advanced() bool { return d.advanced }

// include adds a project to the draft's projects.
func (d *SourceDraft) include(name string) {
	for _, p := range d.Projects {
		if p == name {
			return
		}
	}
	d.Projects = append(d.Projects, name)
}

// isNew reports whether the project has no route in the source as it was.
func (d *SourceDraft) isNew(project string) bool {
	if d.src == nil {
		return true
	}
	for _, r := range d.src.Routes {
		if r.Project == project {
			return false
		}
	}
	return true
}

// asked are the projects whose routes the form asks for: all of them, or only the new ones when the routes
// are kept as written.
func (d *SourceDraft) asked() []string {
	var out []string
	for _, p := range d.Projects {
		if !d.advanced || d.isNew(p) {
			out = append(out, p)
		}
	}
	return out
}

func (d *SourceDraft) value(project string) *string {
	if v, ok := d.Values[project]; ok {
		return v
	}
	v := ""
	d.Values[project] = &v
	return &v
}

// Property is the property the routes test.
func (d *SourceDraft) Property() string {
	if d.By == otherProperty {
		return strings.TrimSpace(d.Other)
	}
	return d.By
}

// Steps are the forms that ask for the source, in order; each is built when it is reached, from the answers
// before it, and a nil form is skipped.
func (d *SourceDraft) Steps(cfg *config.Config) []func() *huh.Form {
	return append([]func() *huh.Form{
		func() *huh.Form { return formOf(d.OfferGroups()) },
		func() *huh.Form { return formOf(d.BasicGroups(cfg)) },
		func() *huh.Form { return formOf(d.BindingGroups(cfg)) },
	}, d.RouteSteps(cfg)...)
}

// RouteSteps ask how the issues are split, and show where the open ones would go.
func (d *SourceDraft) RouteSteps(cfg *config.Config) []func() *huh.Form {
	return []func() *huh.Form{
		func() *huh.Form { return formOf(d.PropertyGroups(cfg)) },
		func() *huh.Form { return formOf(d.ValueGroups(cfg)) },
		func() *huh.Form { return formOf(d.PreviewGroups(cfg)) },
	}
}

func formOf(groups []*huh.Group) *huh.Form {
	if len(groups) == 0 {
		return nil
	}
	return huh.NewForm(groups...)
}

// OfferGroups ask the offer, if there is one.
func (d *SourceDraft) OfferGroups() []*huh.Group {
	if d.Offer == "" {
		return nil
	}
	return []*huh.Group{huh.NewGroup(huh.NewConfirm().Title("Share the tracker?").Description(d.Offer).Affirmative("Share").Negative("Keep them apart").Value(&d.Accepted))}
}

// BasicGroups ask for the source's name, tracker, projects and what happens to issues nothing takes.
func (d *SourceDraft) BasicGroups(cfg *config.Config) []*huh.Group {
	if !d.Accepted {
		return nil
	}
	var trackers []huh.Option[string]
	for _, n := range cfg.ConnectionNames(adapter.RoleTracker) {
		trackers = append(trackers, huh.NewOption(fmt.Sprintf("%s (%s)", n, cfg.Connections[n].Type), n))
	}
	if len(trackers) == 0 {
		trackers = append(trackers, huh.NewOption("(add an issue tracker connection first)", ""))
	}
	var projects []huh.Option[string]
	for _, p := range cfg.Projects {
		from := "its own tracker"
		if src := cfg.SourceFor(p); src == nil {
			from = "no issues yet"
		} else if !src.Implicit() {
			from = "via " + src.Name
		} else if src.Tracker != nil {
			from = src.Tracker.Connection
		}
		on := false
		for _, n := range d.Projects {
			on = on || n == p.Name
		}
		projects = append(projects, huh.NewOption(fmt.Sprintf("%s (%s)", p.Name, from), p.Name).Selected(on))
	}
	labels := cfg.Settings.Labels.Unrouted
	return []*huh.Group{huh.NewGroup(
		huh.NewInput().Title("Name *").Description("Used in the TUI, the log and the webhook URL.").Value(&d.Name).Validate(func(s string) error {
			s = strings.TrimSpace(s)
			if !validName(s) {
				return errors.New("letters, digits, dot, dash and underscore only")
			}
			if s != d.Original && (findSource(cfg, s) != nil || cfg.Project(s) != nil) {
				return errors.New("a source or project already has that name")
			}
			return nil
		}),
		huh.NewSelect[string]().Title("Issue tracker").Options(trackers...).Value(&d.Tracker),
		huh.NewMultiSelect[string]().Title("Projects it feeds").Description("Each issue goes to one of them, by the routes asked for next.").
			Options(projects...).Value(&d.Projects).Validate(func(v []string) error {
			if len(v) == 0 {
				return errors.New("pick at least one project")
			}
			return nil
		}),
		huh.NewSelect[string]().Title("Issues no route takes").Options(
			huh.NewOption("leave them alone (they're listed in the TUI)", config.UnroutedIgnore),
			huh.NewOption("label them "+labels, config.UnroutedLabel),
		).Value(&d.Unrouted),
	).Title("Shared issue tracker")}
}

// BindingGroups ask which of the tracker's issues the source reads (a team, a project key).
func (d *SourceDraft) BindingGroups(cfg *config.Config) []*huh.Group {
	d.TrackerFields = nil
	if !d.Accepted {
		return nil
	}
	c, ok := cfg.Connections[d.Tracker]
	if !ok {
		return nil
	}
	spec, ok := adapter.Lookup(c.Type)
	if !ok || len(spec.Binding[adapter.RoleTracker]) == 0 {
		return nil
	}
	values := d.trackerValues
	if d.src != nil && d.src.Tracker != nil && d.src.Tracker.Connection != d.Tracker {
		values = nil
	}
	d.TrackerFields = New(d.Tracker, spec.Binding[adapter.RoleTracker], values)
	return d.TrackerFields.Groups("Issues on " + d.Tracker)
}

// binding is the tracker binding as answered so far.
func (d *SourceDraft) binding() adapter.Values {
	if d.TrackerFields != nil {
		return d.TrackerFields.Current()
	}
	return copyValues(d.trackerValues)
}

// load reads the tracker's open issues once per binding, to show what the routes can test and where the
// issues would go. A failure is kept and shown; the routes can still be typed in.
func (d *SourceDraft) load(cfg *config.Config) {
	key := TrackerKey(cfg, d.Tracker, d.binding())
	if key == d.loadedFor {
		return
	}
	d.loadedFor, d.issues, d.loadErr = key, nil, nil
	env := adapter.Env{
		Log:     func(string, ...any) {},
		Secret:  config.ResolveSecret,
		DataDir: config.DataDir(),
		HTTP:    &http.Client{Timeout: 15 * time.Second},
	}
	src := &config.Source{Name: d.Name, Tracker: &config.Binding{Connection: d.Tracker, Values: d.binding()}}
	tracker, err := engine.BindSource(cfg, src, engine.OpenConnections(cfg, env))
	if err != nil {
		d.loadErr = err
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	d.issues, d.loadErr = tracker.Candidates(ctx)
}

// property is one property seen on the open issues.
type property struct {
	name   string
	issues int
	values []string // most common first
	counts map[string]int
}

// properties are the properties the open issues carry, labels first, then the most widely set.
func properties(issues []core.Issue) []property {
	byName := map[string]*property{}
	add := func(name string, vals []string) {
		if len(vals) == 0 {
			return
		}
		p := byName[name]
		if p == nil {
			p = &property{name: name, counts: map[string]int{}}
			byName[name] = p
		}
		p.issues++
		for _, v := range vals {
			p.counts[v]++
		}
	}
	for _, i := range issues {
		add("labels", i.Labels)
		for k, v := range i.Attrs {
			add(k, v)
		}
	}
	var out []property
	for _, p := range byName {
		for v := range p.counts {
			p.values = append(p.values, v)
		}
		sort.Slice(p.values, func(a, b int) bool {
			if p.counts[p.values[a]] != p.counts[p.values[b]] {
				return p.counts[p.values[a]] > p.counts[p.values[b]]
			}
			return p.values[a] < p.values[b]
		})
		out = append(out, *p)
	}
	sort.Slice(out, func(a, b int) bool {
		if (out[a].name == "labels") != (out[b].name == "labels") {
			return out[a].name == "labels"
		}
		if out[a].issues != out[b].issues {
			return out[a].issues > out[b].issues
		}
		return out[a].name < out[b].name
	})
	return out
}

func (p property) sample(n int) string {
	var parts []string
	for i, v := range p.values {
		if i == n {
			parts = append(parts, fmt.Sprintf("+%d more", len(p.values)-n))
			break
		}
		parts = append(parts, fmt.Sprintf("%s (%d)", v, p.counts[v]))
	}
	return strings.Join(parts, ", ")
}

// PropertyGroups ask which property splits the issues, showing the ones the open issues carry.
func (d *SourceDraft) PropertyGroups(cfg *config.Config) []*huh.Group {
	if !d.Accepted || len(d.asked()) == 0 {
		return nil
	}
	d.load(cfg)
	props := properties(d.issues)
	var opts []huh.Option[string]
	known := map[string]bool{}
	for _, p := range props {
		known[p.name] = true
		opts = append(opts, huh.NewOption(fmt.Sprintf("%-12s %s", p.name, p.sample(4)), p.name))
	}
	if !known["labels"] {
		opts = append([]huh.Option[string]{huh.NewOption("labels", "labels")}, opts...)
		known["labels"] = true
	}
	if d.By != "" && !known[d.By] && d.By != otherProperty {
		d.Other, d.By = d.By, otherProperty
	}
	if d.By == "" {
		d.By = "labels"
	}
	opts = append(opts, huh.NewOption("something else (type its name)", otherProperty))
	desc := fmt.Sprintf("What tells which project an issue belongs to? Seen on the %d open issues:", len(d.issues))
	if d.loadErr != nil {
		desc = "Couldn't read the open issues (" + d.loadErr.Error() + "), so pick or type the property."
	} else if len(d.issues) == 0 {
		desc = "There are no open issues to look at; pick or type the property."
	}
	if d.advanced {
		desc = "The routes already there are kept as written; this is for the new projects. " + desc
	}
	return []*huh.Group{
		huh.NewGroup(huh.NewSelect[string]().Title("Split the issues by").Description(desc).Options(opts...).Value(&d.By)).Title("Routes"),
		huh.NewGroup(huh.NewInput().Title("Property").Description("The name the tracker gives it: see `open-supervisor route <issue>` for an issue's properties.").
			Value(&d.Other).Validate(func(s string) error {
			if strings.TrimSpace(s) == "" {
				return errors.New("the property's name")
			}
			return nil
		})).WithHideFunc(func() bool { return d.By != otherProperty }),
	}
}

// ValueGroups ask, for each project, the values that send an issue to it.
func (d *SourceDraft) ValueGroups(cfg *config.Config) []*huh.Group {
	asked := d.asked()
	if !d.Accepted || len(asked) == 0 {
		return nil
	}
	by := d.Property()
	var seen *property
	for _, p := range properties(d.issues) {
		if strings.EqualFold(p.name, by) {
			p := p
			seen = &p
		}
	}
	intro := "Comma-separated; * and ? are wildcards and case doesn't matter. Leave one project empty to send it everything the others don't take."
	if seen != nil {
		intro += "\n\nSeen: " + seen.sample(12)
	}
	fields := []huh.Field{huh.NewNote().Title("Which " + by + " go where?").Description(intro)}
	for _, name := range asked {
		v := d.value(name)
		if *v == "" && seen != nil && d.isNew(name) {
			// A value named after the project is the likely answer.
			for _, x := range seen.values {
				if strings.EqualFold(x, name) {
					*v = x
				}
			}
		}
		fields = append(fields, huh.NewInput().Title(name).Value(v))
	}
	return []*huh.Group{huh.NewGroup(fields...).Title("Routes")}
}

// PreviewGroups show where the open issues would go.
func (d *SourceDraft) PreviewGroups(cfg *config.Config) []*huh.Group {
	if !d.Accepted {
		return nil
	}
	return []*huh.Group{huh.NewGroup(huh.NewNote().Title("Where the open issues would go").Description(d.Preview(cfg)).Next(true).NextLabel("OK"))}
}

// Preview is where the tracker's open issues would go by the draft's routes.
func (d *SourceDraft) Preview(cfg *config.Config) string {
	routes, err := d.Routes()
	if err != nil {
		return "✗ " + err.Error()
	}
	d.load(cfg)
	if d.loadErr != nil {
		return "Couldn't read the open issues: " + d.loadErr.Error()
	}
	src := &config.Source{Routes: routes}
	counts := map[string]int{}
	var unrouted []core.Issue
	for _, i := range d.issues {
		if n := src.RouteFor(i.Attr); n >= 0 {
			counts[routes[n].Project]++
		} else {
			unrouted = append(unrouted, i)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d open issues\n\n", len(d.issues))
	shown := map[string]bool{}
	for _, r := range routes {
		if shown[r.Project] {
			continue
		}
		shown[r.Project] = true
		when := r.When.String()
		if r.When.Empty() {
			when = "everything else"
		}
		fmt.Fprintf(&b, "  %-16s %4d   %s\n", r.Project, counts[r.Project], when)
	}
	fmt.Fprintf(&b, "  %-16s %4d\n", "nothing takes", len(unrouted))
	for n, i := range unrouted {
		if n == 5 {
			fmt.Fprintf(&b, "    …and %d more\n", len(unrouted)-n)
			break
		}
		fmt.Fprintf(&b, "    %s %s\n", i.Key, i.Title)
	}
	return strings.TrimRight(b.String(), "\n")
}

// Routes are the source's routes as answered: the kept routes first when they were written by hand, every
// project's own, and the one taking everything else last.
func (d *SourceDraft) Routes() ([]config.Route, error) {
	on := map[string]bool{}
	for _, p := range d.Projects {
		on[p] = true
	}
	if len(on) == 0 {
		return nil, errors.New("a source feeds at least one project")
	}
	by := d.Property()
	var routes []config.Route
	var rest *config.Route
	if d.advanced {
		for _, r := range d.src.Routes {
			if !on[r.Project] {
				continue
			}
			r := config.Route{Project: r.Project, When: r.When.Clone()}
			if r.When.Empty() {
				rest = &r
			} else {
				routes = append(routes, r)
			}
		}
	}
	// The source's own order first, then the new projects in the config's.
	var order []string
	if d.src != nil {
		for _, r := range d.src.Routes {
			if on[r.Project] && !contains(order, r.Project) {
				order = append(order, r.Project)
			}
		}
	}
	for _, p := range d.Projects {
		if !contains(order, p) {
			order = append(order, p)
		}
	}
	for _, name := range order {
		if d.advanced && !d.isNew(name) {
			continue
		}
		vals := splitList(*d.value(name))
		if len(vals) == 0 {
			if rest != nil {
				return nil, fmt.Errorf("%s and %s would both take everything else; give one of them values", rest.Project, name)
			}
			rest = &config.Route{Project: name}
			continue
		}
		if by == "" {
			return nil, errors.New("which property do the routes test?")
		}
		routes = append(routes, config.Route{Project: name, When: &config.Match{Fields: map[string][]string{by: vals}}})
	}
	if rest != nil {
		routes = append(routes, *rest)
	}
	return routes, nil
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// Apply writes the source into cfg: the projects it feeds lose their own trackers and any route from another
// source, and a project no longer fed by it gets the tracker as its own. The note says what moved.
func (d *SourceDraft) Apply(cfg *config.Config) (string, error) {
	if !d.Accepted {
		return "", nil
	}
	name := strings.TrimSpace(d.Name)
	if !validName(name) {
		return "", fmt.Errorf("a source needs a name: letters, digits, dot, dash and underscore")
	}
	if name != d.Original && (findSource(cfg, name) != nil || cfg.Project(name) != nil) {
		return "", fmt.Errorf("a source or project is already called %s", name)
	}
	if _, ok := cfg.Connections[d.Tracker]; !ok {
		return "", errors.New("a source needs an issue tracker connection")
	}
	values := d.binding()
	if d.TrackerFields != nil {
		v, err := d.TrackerFields.Apply()
		if err != nil {
			return "", err
		}
		values = v
	}
	routes, err := d.Routes()
	if err != nil {
		return "", err
	}
	for _, r := range routes {
		if cfg.Project(r.Project) == nil {
			return "", fmt.Errorf("no project called %s", r.Project)
		}
	}
	src := findSource(cfg, d.Original)
	if src == nil || d.Original == "" {
		src = &config.Source{}
		cfg.SourceList = append(cfg.SourceList, src)
	}
	before := map[string]bool{}
	for _, r := range src.Routes {
		before[r.Project] = true
	}
	src.Name = name
	src.Tracker = &config.Binding{Connection: d.Tracker, Values: values}
	src.Routes = routes
	src.Unrouted = d.Unrouted
	if src.Unrouted == config.UnroutedIgnore {
		src.Unrouted = ""
	}
	var notes []string
	fed := map[string]bool{}
	for _, r := range routes {
		fed[r.Project] = true
	}
	for _, p := range cfg.Projects {
		switch {
		case fed[p.Name]:
			dropRoutes(cfg, p.Name, src)
			if p.Labels != nil {
				// The claimed and skip labels live on the shared tracker, so they belong to the source.
				if src.Labels == nil {
					src.Labels = p.Labels
				}
				p.Labels = nil
			}
			p.Tracker = nil
		case before[p.Name]:
			// Out of the source: it reads the tracker on its own again.
			p.Tracker = &config.Binding{Connection: d.Tracker, Values: copyValues(values)}
			notes = append(notes, p.Name+" reads "+d.Tracker+" on its own again")
		}
	}
	return strings.Join(notes, "; "), nil
}

// RemoveSource takes a source out of the config; each project it fed reads the tracker on its own again.
func RemoveSource(cfg *config.Config, name string) {
	for i, s := range cfg.SourceList {
		if s.Name != name {
			continue
		}
		for _, r := range s.Routes {
			if p := cfg.Project(r.Project); p != nil && p.Tracker == nil && s.Tracker != nil {
				p.Tracker = &config.Binding{Connection: s.Tracker.Connection, Values: copyValues(s.Tracker.Values)}
				if p.Labels == nil && s.Labels != nil {
					l := *s.Labels
					p.Labels = &l
				}
			}
		}
		cfg.SourceList = append(cfg.SourceList[:i], cfg.SourceList[i+1:]...)
		return
	}
}

// dropRoutes takes the routes to a project out of every source but keep, and drops a source left with none.
func dropRoutes(cfg *config.Config, project string, keep *config.Source) {
	var sources []*config.Source
	for _, src := range cfg.SourceList {
		emptied := false
		if src != keep {
			var routes []config.Route
			for _, r := range src.Routes {
				if r.Project != project {
					routes = append(routes, r)
				}
			}
			// Only a source this took the last route from goes; one still being set up keeps its place.
			emptied = len(routes) == 0 && len(src.Routes) > 0
			src.Routes = routes
		}
		if !emptied {
			sources = append(sources, src)
		}
	}
	cfg.SourceList = sources
}

// TrackerKey identifies the issues a tracker binding reads, so two projects reading the same ones can be told:
// the connection and every binding value, the adapter's defaults filled in.
func TrackerKey(cfg *config.Config, conn string, values adapter.Values) string {
	v := adapter.Values{}
	if c, ok := cfg.Connections[conn]; ok {
		if spec, ok := adapter.Lookup(c.Type); ok {
			for _, f := range spec.Binding[adapter.RoleTracker] {
				if f.Default != "" {
					v[f.Key] = f.Default
				}
			}
		}
	}
	for k, x := range values {
		if x = strings.TrimSpace(x); x != "" {
			v[k] = x
		}
	}
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{conn}
	for _, k := range keys {
		parts = append(parts, k+"="+strings.ToLower(v[k]))
	}
	return strings.Join(parts, "\x00")
}

// OwnTracker is the binding a project's own tracker reads.
func OwnTracker(p *config.Project) adapter.Values { return ownTracker(p) }

// ownTracker is the binding a project's own tracker reads: on the code host's connection, the repo's binding
// with the tracker's laid over it.
func ownTracker(p *config.Project) adapter.Values {
	if p.Repo != nil && p.Repo.Connection == p.Tracker.Connection {
		out := copyValues(p.Repo.Values)
		for k, v := range p.Tracker.Values {
			if v != "" {
				out[k] = v
			}
		}
		return out
	}
	return p.Tracker.Values
}

// describeBinding is a tracker binding for people: "linear, team ENG".
func describeBinding(conn string, values adapter.Values) string {
	keys := make([]string, 0, len(values))
	for k, v := range values {
		if v != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+" "+values[k])
	}
	if len(parts) == 0 {
		return conn
	}
	return conn + ", " + strings.Join(parts, ", ")
}

// Follow is what a project just written into cfg needs from a source: the routes to it when it was set to
// take its issues from one, or, when its own tracker reads the same issues as another project or a source,
// the offer to share them. Nil when it needs nothing.
func (d *ProjectDraft) Follow(cfg *config.Config, p *config.Project) *SourceDraft {
	if name, ok := strings.CutPrefix(d.Tracker, FromSource); ok {
		src := findSource(cfg, name)
		if src == nil {
			return nil
		}
		for _, r := range src.Routes {
			if r.Project == p.Name {
				return nil
			}
		}
		sd := NewSourceDraft(cfg, src)
		sd.include(p.Name)
		return sd
	}
	return SharedTracker(cfg, p)
}

// SharedTracker offers to share a project's tracker when it reads the same issues as a source or another
// project, which would otherwise both claim them. Nil when nothing else reads them.
func SharedTracker(cfg *config.Config, p *config.Project) *SourceDraft {
	if p.Tracker == nil {
		return nil
	}
	values := ownTracker(p)
	key := TrackerKey(cfg, p.Tracker.Connection, values)
	for _, src := range cfg.SourceList {
		if src.Tracker != nil && TrackerKey(cfg, src.Tracker.Connection, src.Tracker.Values) == key {
			sd := NewSourceDraft(cfg, src)
			sd.include(p.Name)
			var fed []string
			for _, r := range src.Routes {
				if !contains(fed, r.Project) {
					fed = append(fed, r.Project)
				}
			}
			sd.Offer = fmt.Sprintf("The source %s already reads these issues (%s) for %s, so %s would claim the same ones. Add %s to %s's routes, so each issue goes to one project?",
				src.Name, describeBinding(src.Tracker.Connection, src.Tracker.Values), strings.Join(fed, ", "), p.Name, p.Name, src.Name)
			sd.Accepted = true
			return sd
		}
	}
	for _, q := range cfg.Projects {
		if q == p || q.Name == p.Name || q.Tracker == nil || q.Tracker.Connection != p.Tracker.Connection {
			continue
		}
		if TrackerKey(cfg, q.Tracker.Connection, ownTracker(q)) != key {
			continue
		}
		sd := NewSourceDraft(cfg, nil)
		sd.Tracker = p.Tracker.Connection
		sd.trackerValues = copyValues(values)
		sd.Name = UniqueSourceName(cfg, p.Tracker.Connection)
		sd.Projects = []string{q.Name, p.Name}
		sd.Offer = fmt.Sprintf("%s already takes its issues from the same place (%s), so both would claim the same ones. Share them through one source, with routes deciding which project takes each issue?",
			q.Name, describeBinding(p.Tracker.Connection, values))
		sd.Accepted = true
		return sd
	}
	return nil
}

// SetTracker points the draft at a tracker binding without the form.
func (d *SourceDraft) SetTracker(conn string, values adapter.Values) {
	d.Tracker, d.trackerValues, d.TrackerFields = conn, copyValues(values), nil
}

// Inspect reads the tracker's open issues and describes them: the properties they carry, with their most
// common values, and where the draft's routes would send them.
func (d *SourceDraft) Inspect(cfg *config.Config) (string, error) {
	d.load(cfg)
	if d.loadErr != nil {
		return "", d.loadErr
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d open issues on %s\n\nproperties (issues carrying it: values)\n", len(d.issues), describeBinding(d.Tracker, d.binding()))
	for _, p := range properties(d.issues) {
		fmt.Fprintf(&b, "  %-14s %4d: %s\n", p.name, p.issues, p.sample(10))
	}
	if len(d.Projects) > 0 {
		preview := d.Preview(cfg)
		if _, rest, ok := strings.Cut(preview, "\n\n"); ok {
			preview = rest // the count is above
		}
		b.WriteString("\nroutes\n" + preview + "\n")
	}
	return b.String(), nil
}

// SetRoute sends a source's issues that match to a project, replacing the routes to it in that source (or,
// with also, adding another). position is where the route goes, from 1; 0 keeps the place of the route it
// replaces, else puts it before the route taking everything else. The project stops reading a tracker of its
// own or any other source's issues; the note says so.
func SetRoute(cfg *config.Config, source, project string, when *config.Match, position int, also bool) (string, error) {
	src := findSource(cfg, source)
	if src == nil {
		return "", fmt.Errorf("no source called %s", source)
	}
	p := cfg.Project(project)
	if p == nil {
		return "", fmt.Errorf("no project called %s", project)
	}
	if when.Empty() {
		when = nil
	}
	at := -1
	var routes []config.Route
	for _, r := range src.Routes {
		if r.Project == project && !also {
			if at < 0 {
				at = len(routes)
			}
			continue
		}
		routes = append(routes, r)
	}
	if when == nil {
		for _, r := range routes {
			if r.When.Empty() {
				return "", fmt.Errorf("%s already takes everything else from %s; give one of them a --when", r.Project, source)
			}
		}
		at = len(routes)
	} else {
		switch {
		case position > 0:
			at = min(position-1, len(routes))
		case at < 0:
			at = len(routes)
		}
		// Never after the route taking everything else, where it could match nothing.
		if n := len(routes); n > 0 && routes[n-1].When.Empty() && at > n-1 {
			at = n - 1
		}
	}
	route := config.Route{Project: project, When: when}
	routes = append(routes[:at], append([]config.Route{route}, routes[at:]...)...)
	src.Routes = routes
	var notes []string
	if p.Tracker != nil {
		notes = append(notes, project+" no longer reads "+p.Tracker.Connection+" on its own")
		p.Tracker = nil
	}
	if p.Labels != nil {
		if src.Labels == nil {
			src.Labels = p.Labels
		}
		p.Labels = nil
	}
	for _, other := range cfg.SourceList {
		if other != src && RoutedFrom(other, project) {
			notes = append(notes, "routes to "+project+" from "+other.Name+" were removed")
		}
	}
	dropRoutes(cfg, project, src)
	return strings.Join(notes, "; "), nil
}

// RemoveRoutes takes a source's routes to a project out. The note says when that leaves it with no issues.
func RemoveRoutes(cfg *config.Config, source, project string) (string, error) {
	src := findSource(cfg, source)
	if src == nil {
		return "", fmt.Errorf("no source called %s", source)
	}
	if !RoutedFrom(src, project) {
		return "", fmt.Errorf("%s has no route to %s", source, project)
	}
	var routes []config.Route
	for _, r := range src.Routes {
		if r.Project != project {
			routes = append(routes, r)
		}
	}
	if len(routes) == 0 {
		RemoveSource(cfg, source)
		return source + " fed nothing else, so it's gone; " + project + " reads its tracker on its own again", nil
	}
	src.Routes = routes
	if p := cfg.Project(project); p != nil && cfg.SourceFor(p) == nil {
		return project + " now gets no issues: give it a tracker or a route", nil
	}
	return "", nil
}

// RoutedFrom reports whether the source has a route to the project.
func RoutedFrom(src *config.Source, project string) bool {
	for _, r := range src.Routes {
		if r.Project == project {
			return true
		}
	}
	return false
}

// BindingArgs are a binding's values as key=value, quoted for a shell where they need it.
func BindingArgs(values adapter.Values) []string {
	keys := make([]string, 0, len(values))
	for k, v := range values {
		if v != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		kv := k + "=" + values[k]
		if strings.ContainsAny(kv, " ,*?'\"$") {
			kv = "'" + strings.ReplaceAll(kv, "'", `'\''`) + "'"
		}
		out = append(out, kv)
	}
	return out
}
