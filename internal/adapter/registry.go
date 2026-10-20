package adapter

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// FieldType is how a field is asked for and stored.
type FieldType string

const (
	Text   FieldType = "text"
	Secret FieldType = "secret" // stored in the keyring (or the secrets file), referenced from the config
	Bool   FieldType = "bool"
	Int    FieldType = "int"
	Select FieldType = "select"
)

// Field is one setting an adapter needs.
type Field struct {
	Key         string
	Label       string
	Help        string
	Type        FieldType
	Required    bool
	Default     string
	Placeholder string
	Options     []string
	// ShowIf shows the field only when another field has one of these values (an auth mode's own fields).
	ShowIf map[string][]string
}

// Visible reports whether the field applies given the other values.
func (f Field) Visible(v Values) bool {
	for key, want := range f.ShowIf {
		got := v.Get(key, "")
		ok := false
		for _, w := range want {
			if got == w {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// Values are a connection's or a binding's settings, as strings.
type Values map[string]string

// Get returns the value or the fallback when it is empty.
func (v Values) Get(key, fallback string) string {
	if s, ok := v[key]; ok && s != "" {
		return s
	}
	return fallback
}

// Int returns the value as an int, or the fallback.
func (v Values) Int(key string, fallback int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(v[key])); err == nil {
		return n
	}
	return fallback
}

// Bool returns the value as a bool, or the fallback.
func (v Values) Bool(key string, fallback bool) bool {
	if b, err := strconv.ParseBool(strings.TrimSpace(v[key])); err == nil {
		return b
	}
	return fallback
}

// Spec describes an adapter type: what it is, the roles it can fill and the settings it needs.
type Spec struct {
	// Type is the name used in the config ("github", "jira", "claude-code").
	Type        string
	Title       string
	Description string
	Roles       []Role
	// Fields are the connection's own settings: credentials, a site URL, a model.
	Fields []Field
	// Binding are the settings a project gives for each role it binds to the connection (a repo name, a
	// project key). Most can be detected; see RepoProvider.DetectRepo.
	Binding map[Role][]Field
	// New opens a connection from its settings.
	New func(v Values, env Env) (Connection, error)
}

// Has reports whether the adapter can fill the role.
func (s Spec) Has(r Role) bool {
	for _, x := range s.Roles {
		if x == r {
			return true
		}
	}
	return false
}

// Defaults are the fields' default values.
func (s Spec) Defaults() Values {
	v := Values{}
	for _, f := range s.Fields {
		if f.Default != "" {
			v[f.Key] = f.Default
		}
	}
	return v
}

// Validate reports the first required field that is missing.
func (s Spec) Validate(v Values) error {
	for _, f := range s.Fields {
		if f.Required && f.Visible(v) && strings.TrimSpace(v[f.Key]) == "" {
			return fmt.Errorf("%s: %s is required", s.Type, f.Label)
		}
	}
	return nil
}

var (
	mu    sync.RWMutex
	specs = map[string]Spec{}
)

// Register adds an adapter type. Adapters call it from init.
func Register(s Spec) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := specs[s.Type]; dup {
		panic("adapter registered twice: " + s.Type)
	}
	specs[s.Type] = s
}

// Lookup finds an adapter type by name.
func Lookup(typ string) (Spec, bool) {
	mu.RLock()
	defer mu.RUnlock()
	s, ok := specs[typ]
	return s, ok
}

// All lists every adapter type, by title.
func All() []Spec {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Spec, 0, len(specs))
	for _, s := range specs {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	return out
}

// ForRole lists the adapter types that can fill a role.
func ForRole(r Role) []Spec {
	var out []Spec
	for _, s := range All() {
		if s.Has(r) {
			out = append(out, s)
		}
	}
	return out
}
