package engine

import (
	"fmt"
	"sync"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/config"
)

// Connections opens the config's connections as they are first needed, once each.
type Connections struct {
	cfg *config.Config
	env adapter.Env

	mu     sync.Mutex
	opened map[string]adapter.Connection
	errs   map[string]error
}

// OpenConnections prepares the config's connections; none is opened until asked for.
func OpenConnections(cfg *config.Config, env adapter.Env) *Connections {
	return &Connections{cfg: cfg, env: env, opened: map[string]adapter.Connection{}, errs: map[string]error{}}
}

// Get opens (or returns the already open) connection by name.
func (c *Connections) Get(name string) (adapter.Connection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if conn, ok := c.opened[name]; ok {
		return conn, nil
	}
	if err, ok := c.errs[name]; ok {
		return nil, err
	}
	conn, err := Open(c.cfg, name, c.env)
	if err != nil {
		c.errs[name] = err
		return nil, err
	}
	c.opened[name] = conn
	return conn, nil
}

// Open opens one connection from the config.
func Open(cfg *config.Config, name string, env adapter.Env) (adapter.Connection, error) {
	def, ok := cfg.Connections[name]
	if !ok {
		return nil, fmt.Errorf("no connection named %q", name)
	}
	spec, ok := adapter.Lookup(def.Type)
	if !ok {
		return nil, fmt.Errorf("connection %s: unknown type %q", name, def.Type)
	}
	values := spec.Defaults()
	for k, v := range def.Values {
		values[k] = v
	}
	conn, err := spec.New(values, env)
	if err != nil {
		return nil, fmt.Errorf("connection %s (%s): %w", name, def.Type, err)
	}
	return conn, nil
}
