// Package engine is the supervisor: one process over every project in the config. It owns what the projects
// share (the webhook server and its tunnel, the cap on turns running at once, the agents' usage windows,
// telemetry and the daily sweep) and hands everything else to each Project. Webhooks make it quick; a
// reconcile every few minutes makes it right.
package engine

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/config"
	"github.com/alsiola/open-supervisor/internal/core"
	"github.com/alsiola/open-supervisor/internal/logx"
	"github.com/alsiola/open-supervisor/internal/playbooks"
	"github.com/alsiola/open-supervisor/internal/store"
)

// Options are how the supervisor was started.
type Options struct {
	DryRun   bool
	NoTunnel bool
}

// Supervisor holds every project.
type Supervisor struct {
	opts    Options
	state   string
	data    string
	exe     string
	secret  string
	changes chan struct{}

	mu       sync.RWMutex
	cfg      *config.Config
	conns    *Connections
	projects []*Project
	sinks    []adapter.Telemetry
	hookURL  string
	usage    map[string]*core.Usage
	usageErr map[string]string
	sweeping bool
	stopping bool

	schedMu sync.Mutex
	server  *http.Server
	cancel  context.CancelFunc
	tunnel  context.CancelFunc
}

// New builds the supervisor from a config.
func New(cfg *config.Config, opts Options) (*Supervisor, error) {
	exe, err := os.Executable()
	if err != nil {
		exe = config.Name
	}
	s := &Supervisor{
		opts:     opts,
		state:    config.StateDir(),
		data:     config.DataDir(),
		exe:      exe,
		changes:  make(chan struct{}, 1),
		usage:    map[string]*core.Usage{},
		usageErr: map[string]string{},
	}
	if opts.DryRun {
		cfg.Settings.DryRun = true
	}
	s.cfg = cfg
	s.conns = OpenConnections(cfg, s.adapterEnv())
	projects, err := s.buildProjects(cfg, nil)
	if err != nil {
		return nil, err
	}
	s.projects = projects
	s.sinks = s.openSinks(cfg)
	return s, nil
}

func (s *Supervisor) adapterEnv() adapter.Env {
	return adapter.Env{
		Log:     func(format string, args ...any) { logx.Default.Printf(format, args...) },
		Secret:  config.ResolveSecret,
		DryRun:  s.opts.DryRun || (s.cfg != nil && s.cfg.Settings.DryRun),
		DataDir: s.data,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

// Config is the config the supervisor runs on.
func (s *Supervisor) Config() *config.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Projects are the projects the supervisor watches.
func (s *Supervisor) Projects() []*Project {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]*Project(nil), s.projects...)
}

// Project finds a project by name.
func (s *Supervisor) Project(name string) *Project {
	for _, p := range s.Projects() {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// Changes is signalled (without blocking) whenever something the TUI shows has changed.
func (s *Supervisor) Changes() <-chan struct{} { return s.changes }

// HookURL is the public webhook URL, empty while the tunnel is down or off.
func (s *Supervisor) HookURL() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hookURL
}

// TunnelEnabled reports whether webhooks are meant to arrive.
func (s *Supervisor) TunnelEnabled() bool {
	return !s.opts.NoTunnel && s.Config().Settings.Tunnel != ""
}

// Usage is each agent's usage windows, by connection name, and the error reading each.
func (s *Supervisor) Usage() (map[string]*core.Usage, map[string]string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u := map[string]*core.Usage{}
	for k, v := range s.usage {
		u[k] = v
	}
	e := map[string]string{}
	for k, v := range s.usageErr {
		e[k] = v
	}
	return u, e
}

// Sweeping reports whether a sweep is running.
func (s *Supervisor) Sweeping() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sweeping
}

// TurnsRunning counts turns running now, across every project.
func (s *Supervisor) TurnsRunning() int {
	n := 0
	for _, p := range s.Projects() {
		n += p.RunningCount()
	}
	return n
}

// DryRun reports whether the supervisor changes nothing outside this machine.
func (s *Supervisor) DryRun() bool { return s.settings().DryRun }

// ---- hub, for projects ----------------------------------------------------------------------------------

func (s *Supervisor) settings() config.Settings { return s.Config().Settings }

func (s *Supervisor) dataDir() string { return s.data }

func (s *Supervisor) changed() {
	select {
	case s.changes <- struct{}{}:
	default:
	}
}

// sessionEnv is the environment for a project's agent sessions and jobs: the supervisor's own, the project's
// .env and env settings, and what the agent needs to call back into the supervisor.
func (s *Supervisor) sessionEnv(p *Project, key string) []string {
	env := map[string]string{}
	order := []string{}
	set := func(k, v string) {
		if _, ok := env[k]; !ok {
			order = append(order, k)
		}
		env[k] = v
	}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			set(k, v)
		}
	}
	for k, v := range readDotEnv(filepath.Join(p.Root, ".env")) {
		set(k, v)
	}
	for k, v := range p.Spec.Env {
		set(k, v)
	}
	set("OPEN_SUPERVISOR", s.exe)
	set("OPEN_SUPERVISOR_CONFIG", s.Config().Path)
	set("OPEN_SUPERVISOR_PROJECT", p.Name)
	if key != "" {
		set("OPEN_SUPERVISOR_ISSUE", key)
		set("OPEN_SUPERVISOR_BRANCH", p.BranchFor(key))
		set("OPEN_SUPERVISOR_WORKTREE", p.WorktreePath(key))
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+env[k])
	}
	return out
}

func (s *Supervisor) capture(name string, props map[string]any) {
	if s.settings().DryRun {
		return
	}
	s.mu.RLock()
	sinks := s.sinks
	s.mu.RUnlock()
	if len(sinks) == 0 {
		return
	}
	host, _ := os.Hostname()
	all := map[string]any{"host": host}
	for k, v := range props {
		all[k] = v
	}
	e := adapter.TelemetryEvent{Name: name, Time: time.Now().UTC(), Props: all}
	for _, sink := range sinks {
		go func(sink adapter.Telemetry) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := sink.Capture(ctx, e); err != nil {
				warnOnce("telemetry: couldn't send " + name + ": " + logx.ErrText(err))
			}
		}(sink)
	}
}

var (
	warnedMu sync.Mutex
	warned   = map[string]bool{}
)

func warnOnce(msg string) {
	warnedMu.Lock()
	defer warnedMu.Unlock()
	if !warned[msg] {
		warned[msg] = true
		logx.Default.Printf("%s (not repeated)", msg)
	}
}

// schedule starts turns while there's room, taking each project's next issue in turn so none waits on
// another's queue.
func (s *Supervisor) schedule() {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	s.mu.RLock()
	stopping := s.stopping
	s.mu.RUnlock()
	if stopping {
		return
	}
	max := s.settings().MaxTurns
	projects := s.Projects()
	queues := make([][]*store.Record, len(projects))
	for i, p := range projects {
		queues[i] = p.candidates()
	}
	for started := true; started; {
		started = false
		for i, p := range projects {
			if s.TurnsRunning() >= max {
				break
			}
			// Reserve the slot before the turn's goroutine starts, so the count above sees it.
			var next *store.Record
			for len(queues[i]) > 0 && next == nil {
				if p.tryStart(queues[i][0].Key) {
					next = queues[i][0]
				}
				queues[i] = queues[i][1:]
			}
			if next == nil {
				continue
			}
			go p.runTurn(next.Key)
			started = true
		}
	}
	s.changed()
}

// ---- lifecycle ------------------------------------------------------------------------------------------

// Start begins watching: projects, the webhook server and tunnel, the timers.
func (s *Supervisor) Start() error {
	if err := os.MkdirAll(s.state, 0o755); err != nil {
		return err
	}
	secret, err := webhookSecret(filepath.Join(s.state, "webhook-secret"))
	if err != nil {
		return err
	}
	s.secret = secret
	cfg := s.Config()
	names := []string{}
	for _, p := range s.Projects() {
		names = append(names, p.Name)
	}
	mode := ""
	if cfg.Settings.DryRun {
		mode = ", dry run"
	}
	logx.Default.Printf("supervisor started: %s; %d turns at once%s", strings.Join(names, ", "), cfg.Settings.MaxTurns, mode)
	s.prepareAgents()
	for _, p := range s.Projects() {
		p.start()
	}
	// Without a tunnel nothing can reach the webhook server, so it isn't started.
	if !s.TunnelEnabled() {
		for _, p := range s.Projects() {
			if len(p.Jobs.Events()) > 0 || len(p.Jobs.Webhooked()) > 0 {
				p.logf("no tunnel: jobs that run on webhooks won't run")
			}
		}
	}
	if s.TunnelEnabled() {
		if err := s.startServer(cfg.Settings.Port); err != nil {
			logx.Default.Printf("%s; webhooks are off until the port is free (set settings.port to another)", logx.ErrText(err))
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.startTunnel()
	go s.loop(ctx)
	go s.readUsage()
	go s.Reconcile()
	return nil
}

// prepareAgents readies every agent the projects use to run the playbooks.
func (s *Supervisor) prepareAgents() {
	done := map[string]bool{}
	for _, p := range s.Projects() {
		if done[p.AgentName] {
			continue
		}
		done[p.AgentName] = true
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		if err := p.Agent.Prepare(ctx, playbooks.All()); err != nil {
			logx.Default.Printf("agent %s: %s", p.AgentName, logx.ErrText(err))
		}
		cancel()
	}
}

func (s *Supervisor) loop(ctx context.Context) {
	minute := time.NewTicker(time.Minute)
	defer minute.Stop()
	lastReconcile := time.Now()
	lastUsage := time.Now()
	lastPrune := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-minute.C:
			st := s.settings()
			if time.Since(lastReconcile) >= time.Duration(st.ReconcileMinutes)*time.Minute {
				lastReconcile = time.Now()
				go s.Reconcile()
			}
			if time.Since(lastUsage) >= time.Duration(st.UsageMinutes)*time.Minute {
				lastUsage = time.Now()
				go s.readUsage()
			}
			if time.Since(lastPrune) >= 6*time.Hour {
				lastPrune = time.Now()
				for _, p := range s.Projects() {
					p.Store.Prune()
				}
			}
			go s.wakeDue()
			go s.sweepIfDue()
			// Failed turns become runnable again as their retry time passes.
			s.schedule()
		}
	}
}

// Stop leaves running turns running (the next start adopts them) and stops everything else.
func (s *Supervisor) Stop() {
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
	if s.tunnel != nil {
		s.tunnel()
	}
	running := s.TurnsRunning()
	if running > 0 {
		logx.Default.Printf("stopping: leaving %d turn(s) running; the next start picks them up", running)
	}
	for _, p := range s.Projects() {
		p.stop()
	}
	if s.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = s.server.Shutdown(ctx)
		cancel()
	}
	s.mu.RLock()
	sinks := s.sinks
	s.mu.RUnlock()
	for _, sink := range sinks {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = sink.Close(ctx)
		cancel()
	}
}

// Reconcile runs every project's reconcile.
func (s *Supervisor) Reconcile() {
	var wg sync.WaitGroup
	for _, p := range s.Projects() {
		wg.Add(1)
		go func(p *Project) {
			defer wg.Done()
			p.reconcile()
		}(p)
	}
	wg.Wait()
}

func (s *Supervisor) wakeDue() {
	for _, p := range s.Projects() {
		p.wakeDue()
	}
}

// readUsage asks each agent that can for its subscription's windows.
func (s *Supervisor) readUsage() {
	seen := map[string]bool{}
	for _, p := range s.Projects() {
		if seen[p.AgentName] {
			continue
		}
		seen[p.AgentName] = true
		r, ok := p.Agent.(adapter.UsageReporter)
		if !ok {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		u, err := r.Usage(ctx)
		cancel()
		if err == nil && u == nil {
			continue // this agent doesn't report usage
		}
		s.mu.Lock()
		if err != nil {
			text := logx.ErrText(err)
			if s.usageErr[p.AgentName] != text {
				logx.Default.Printf("usage: couldn't read %s's windows: %s", p.AgentName, text)
			}
			s.usageErr[p.AgentName] = text
			s.mu.Unlock()
			continue
		}
		old := s.usage[p.AgentName]
		s.usage[p.AgentName] = u
		delete(s.usageErr, p.AgentName)
		s.mu.Unlock()
		if !sameWindows(old, u) {
			props := map[string]any{"agent": p.AgentName, "turns_running": s.TurnsRunning()}
			for _, w := range u.Windows {
				key := strings.NewReplacer("h", "_hour", "d", "_day").Replace(w.Name)
				props[key+"_pct"] = w.Used
				props[key+"_resets_at"] = w.ResetsAt
			}
			s.capture("agent_usage", props)
		}
	}
	s.changed()
}

func sameWindows(a, b *core.Usage) bool {
	if a == nil || b == nil {
		return a == b
	}
	x, _ := json.Marshal(a.Windows)
	y, _ := json.Marshal(b.Windows)
	return string(x) == string(y)
}

// sweepIfDue runs, once a day at sweepAt, the sweep of every project not yet swept today, one after another.
func (s *Supervisor) sweepIfDue() {
	at := s.settings().SweepAt
	if at == "" {
		return
	}
	now := time.Now()
	var h, m int
	if _, err := fmt.Sscanf(at, "%d:%d", &h, &m); err != nil {
		return
	}
	if now.Hour() < h || (now.Hour() == h && now.Minute() < m) {
		return
	}
	today := now.Format("2006-01-02")
	var due []*Project
	for _, p := range s.Projects() {
		if p.Store.LastSweep() != today && !p.Spec.Sweep.Disabled {
			due = append(due, p)
		}
	}
	if len(due) == 0 || s.Sweeping() {
		return
	}
	for _, p := range due {
		p.Store.SetLastSweep(today)
	}
	s.RunSweep(due)
}

// RunSweep sweeps the projects (every one when none are given), one after another.
func (s *Supervisor) RunSweep(projects []*Project) {
	s.mu.Lock()
	if s.sweeping {
		s.mu.Unlock()
		return
	}
	s.sweeping = true
	s.mu.Unlock()
	s.changed()
	defer func() {
		s.mu.Lock()
		s.sweeping = false
		s.mu.Unlock()
		s.changed()
	}()
	if projects == nil {
		projects = s.Projects()
	}
	for _, p := range projects {
		p.runSweep()
	}
}

// ---- webhooks -------------------------------------------------------------------------------------------

func webhookSecret(file string) (string, error) {
	if data, err := os.ReadFile(file); err == nil {
		return strings.TrimSpace(string(data)), nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	secret := hex.EncodeToString(b)
	return secret, os.WriteFile(file, []byte(secret+"\n"), 0o600)
}

var (
	hookPath = regexp.MustCompile(`^/hooks/([^/]+)/([^/]+)$`)
	jobPath  = regexp.MustCompile(`^/jobs/([^/]+)/([^/]+)/([^/]+)$`)
)

// jobToken is the secret part of a job's webhook URL: stable across restarts, and different for every job.
func jobToken(secret, project, job string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("job:" + project + "/" + job))
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// JobHookURL is where a job that takes webhooks takes them, empty while the tunnel is down.
func (s *Supervisor) JobHookURL(p *Project, job string) string {
	base := s.HookURL()
	if base == "" {
		return ""
	}
	return fmt.Sprintf("%s/jobs/%s/%s/%s", strings.TrimRight(base, "/"), url.PathEscape(p.Name), url.PathEscape(job), jobToken(s.secret, p.Name, job))
}

// announceJobHooks logs the URL of each of the project's jobs that take webhooks.
func (s *Supervisor) announceJobHooks(p *Project) {
	for _, job := range p.Jobs.Webhooked() {
		if u := s.JobHookURL(p, job.Name); u != "" {
			p.logf("%s takes webhooks at %s", job.Name, u)
		}
	}
}

func (s *Supervisor) startServer(port int) error {
	ln := fmt.Sprintf("127.0.0.1:%d", port)
	s.server = &http.Server{Addr: ln, Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- s.server.ListenAndServe() }()
	select {
	case err := <-errc:
		return fmt.Errorf("webhook server on %s: %w", ln, err)
	case <-time.After(150 * time.Millisecond):
	}
	logx.Default.Printf("webhook server on http://%s/hooks/<project>/<connection>", ln)
	return nil
}

// handler is the webhook server: the projects' hooks at /hooks/<project>/<connection>, and jobs that take
// webhooks at /jobs/<project>/<job>/<token>.
func (s *Supervisor) handler() http.Handler {
	mux := http.NewServeMux()
	seen := newRecent(2000)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "open-supervisor ok") })
	mux.HandleFunc("/hooks/", func(w http.ResponseWriter, r *http.Request) {
		m := hookPath.FindStringSubmatch(r.URL.Path)
		if r.Method != http.MethodPost || m == nil {
			http.NotFound(w, r)
			return
		}
		p := s.Project(m[1])
		if p == nil {
			http.NotFound(w, r)
			return
		}
		body, err := readBody(r)
		if err != nil {
			http.Error(w, "too big", http.StatusRequestEntityTooLarge)
			return
		}
		for _, h := range []string{"X-GitHub-Delivery", "X-Request-UUID", "X-Atlassian-Webhook-Identifier", "X-Hook-UUID"} {
			if id := r.Header.Get(h); id != "" {
				if seen.has(m[2] + id) {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				seen.add(m[2] + id)
				break
			}
		}
		var signals []core.Signal
		ok := false
		for _, hooked := range p.hookedBy(m[2]) {
			sigs, err := hooked.ParseWebhook(r.Header, body, s.secret)
			if err != nil {
				p.logf("webhook: %s", logx.ErrText(err))
				continue
			}
			ok = true
			signals = append(signals, sigs...)
		}
		if !ok {
			http.Error(w, "rejected", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		go func() {
			event := false
			for _, sig := range signals {
				if sig.Type == core.SigEvent {
					// The tracker and the repo on one hook both send the delivery; jobs run on it once.
					if event || sig.Event == nil {
						continue
					}
					event = true
					p.Jobs.OnEvent(*sig.Event)
					continue
				}
				p.onSignal(sig)
			}
		}()
	})
	mux.HandleFunc("/jobs/", func(w http.ResponseWriter, r *http.Request) {
		m := jobPath.FindStringSubmatch(r.URL.Path)
		if r.Method != http.MethodPost || m == nil {
			http.NotFound(w, r)
			return
		}
		p := s.Project(m[1])
		if p == nil {
			http.NotFound(w, r)
			return
		}
		job, ok := p.Jobs.Find(m[2])
		if !ok || job.On == nil || !job.On.Webhook || !hmac.Equal([]byte(m[3]), []byte(jobToken(s.secret, p.Name, job.Name))) {
			http.NotFound(w, r)
			return
		}
		body, err := readBody(r)
		if err != nil {
			http.Error(w, "too big", http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		go p.Jobs.Trigger(job, core.HookEvent{Name: "webhook", Body: body})
	})
	return mux
}

func readBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(http.MaxBytesReader(nil, r.Body, 25<<20))
}

// hookedBy are the project's tracker and repo bound to a connection that take webhooks.
func (p *Project) hookedBy(conn string) []adapter.Hooked {
	var out []adapter.Hooked
	if conn == p.trackerConn {
		if h, ok := p.Tracker.(adapter.Hooked); ok {
			out = append(out, h)
		}
	}
	if conn == p.repoConn {
		if h, ok := p.Repo.(adapter.Hooked); ok {
			out = append(out, h)
		}
	}
	return out
}

// pointHooks points each of the project's hooks at the public URL; a tracker and a repo on the same target
// share one.
func (p *Project) pointHooks(base, secret string) {
	type hook struct {
		h    adapter.Hooked
		conn string
	}
	var hooks []hook
	if h, ok := p.Tracker.(adapter.Hooked); ok {
		hooks = append(hooks, hook{h, p.trackerConn})
	}
	if h, ok := p.Repo.(adapter.Hooked); ok {
		hooks = append(hooks, hook{h, p.repoConn})
	}
	done := map[string]bool{}
	for _, x := range hooks {
		target := x.h.HookTarget()
		if done[target] {
			continue
		}
		done[target] = true
		url := fmt.Sprintf("%s/hooks/%s/%s", strings.TrimRight(base, "/"), p.Name, x.conn)
		existing, _ := p.Store.Hook(target)
		ctx, cancel := ctxTimeout(time.Minute)
		id, err := x.h.EnsureWebhook(ctx, url, secret, existing.ID)
		cancel()
		if err != nil {
			p.logf("webhook: couldn't point %s's hook at %s: %s", target, url, logx.ErrText(err))
			continue
		}
		p.Store.SetHook(target, store.Hook{ID: id, URL: url})
		p.logf("webhook: %s's hook %s points at %s", target, id, url)
	}
	p.hub.changed()
}

func (s *Supervisor) startTunnel() {
	cfg := s.Config()
	if s.opts.NoTunnel || cfg.Settings.Tunnel == "" {
		logx.Default.Printf("no tunnel: events arrive only through the reconcile, every %d minutes", cfg.Settings.ReconcileMinutes)
		return
	}
	conn, err := s.conns.Get(cfg.Settings.Tunnel)
	if err != nil {
		logx.Default.Printf("tunnel: %s", logx.ErrText(err))
		return
	}
	t, ok := conn.(adapter.Tunnel)
	if !ok {
		logx.Default.Printf("tunnel: %s can't be a tunnel", cfg.Settings.Tunnel)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.tunnel = cancel
	urls := make(chan string, 4)
	go func() {
		if err := t.Run(ctx, cfg.Settings.Port, urls); err != nil && ctx.Err() == nil {
			logx.Default.Printf("tunnel: %s", logx.ErrText(err))
		}
		close(urls)
	}()
	go func() {
		for url := range urls {
			s.mu.Lock()
			s.hookURL = url
			s.mu.Unlock()
			if url == "" {
				s.changed()
				continue
			}
			logx.Default.Printf("tunnel: %s", url)
			for _, p := range s.Projects() {
				go p.pointHooks(url, s.secret)
				s.announceJobHooks(p)
			}
		}
	}()
}

// recent is a bounded set of delivery ids, so a redelivered webhook is handled once.
type recent struct {
	mu    sync.Mutex
	max   int
	order []string
	set   map[string]bool
}

func newRecent(max int) *recent { return &recent{max: max, set: map[string]bool{}} }

func (r *recent) has(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.set[id]
}

func (r *recent) add(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.set[id] = true
	r.order = append(r.order, id)
	if len(r.order) > r.max {
		delete(r.set, r.order[0])
		r.order = r.order[1:]
	}
}

// ---- reload ---------------------------------------------------------------------------------------------

// Reload applies a changed config without a restart. Settings take effect at once; a project whose definition
// or connections changed is rebuilt (its running turns carry on and are adopted), the rest are kept as they are.
func (s *Supervisor) Reload(cfg *config.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if s.opts.DryRun {
		cfg.Settings.DryRun = true
	}
	old := s.Config()
	conns := OpenConnections(cfg, s.adapterEnv())
	keep := map[string]*Project{}
	for _, p := range s.Projects() {
		keep[p.fingerprint] = p
	}
	s.mu.Lock()
	prevConns := s.conns
	s.cfg = cfg
	s.conns = conns
	s.mu.Unlock()
	projects, err := s.buildProjects(cfg, keep)
	if err != nil {
		s.mu.Lock()
		s.cfg = old
		s.conns = prevConns
		s.mu.Unlock()
		return err
	}
	kept := map[*Project]bool{}
	for _, p := range projects {
		kept[p] = true
	}
	for _, p := range s.Projects() {
		if !kept[p] {
			p.stop()
			p.logf("stopped for the new config")
		}
	}
	s.mu.Lock()
	s.projects = projects
	for _, sink := range s.sinks {
		go sink.Close(context.Background())
	}
	s.sinks = s.openSinks(cfg)
	hook := s.hookURL
	s.mu.Unlock()
	s.prepareAgents()
	for _, p := range projects {
		if !p.begun {
			p.start()
			if hook != "" {
				go p.pointHooks(hook, s.secret)
				s.announceJobHooks(p)
			}
			go p.reconcile()
		}
	}
	if old.Settings.Tunnel != cfg.Settings.Tunnel || old.Settings.Port != cfg.Settings.Port {
		logx.Default.Printf("the tunnel and the webhook port change at the next start")
	}
	logx.Default.Printf("config reloaded: %d project(s), %d rebuilt", len(projects), len(projects)-countKept(projects, keep))
	s.schedule()
	return nil
}

func countKept(projects []*Project, keep map[string]*Project) int {
	n := 0
	for _, p := range projects {
		if keep[p.fingerprint] == p {
			n++
		}
	}
	return n
}

// buildProjects opens each enabled project in the config, reusing those in keep whose fingerprint is unchanged.
func (s *Supervisor) buildProjects(cfg *config.Config, keep map[string]*Project) ([]*Project, error) {
	var out []*Project
	var errs []string
	for _, spec := range cfg.Projects {
		if spec.Disabled {
			continue
		}
		fp := fingerprint(cfg, spec)
		if p := keep[fp]; p != nil {
			out = append(out, p)
			continue
		}
		p, err := s.openProject(cfg, spec, fp)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %s", spec.Name, logx.ErrText(err)))
			continue
		}
		out = append(out, p)
	}
	if len(errs) > 0 && len(out) == 0 && len(cfg.Projects) > 0 {
		return nil, fmt.Errorf("no project could be opened:\n  %s", strings.Join(errs, "\n  "))
	}
	for _, e := range errs {
		logx.Default.Printf("project %s", e)
	}
	sort.SliceStable(out, func(i, j int) bool { return indexOf(cfg, out[i].Name) < indexOf(cfg, out[j].Name) })
	return out, nil
}

func indexOf(cfg *config.Config, name string) int {
	for i, p := range cfg.Projects {
		if p.Name == name {
			return i
		}
	}
	return len(cfg.Projects)
}

// fingerprint is everything a project is built from, so a reload can tell whether it changed.
func fingerprint(cfg *config.Config, p *config.Project) string {
	conns := map[string]*config.Connection{}
	agent := cfg.AgentFor(p)
	for _, name := range []string{agent, p.Repo.Connection, p.Tracker.Connection} {
		conns[name] = cfg.Connections[name]
	}
	b, _ := json.Marshal(struct {
		P      *config.Project
		C      map[string]*config.Connection
		L      config.Labels
		B, Agt string
	}{p, conns, cfg.LabelsFor(p), cfg.BranchFor(p), agent})
	return string(b)
}

func (s *Supervisor) openProject(cfg *config.Config, spec *config.Project, fp string) (*Project, error) {
	b, err := Bind(cfg, spec, s.conns, s.state)
	if err != nil {
		return nil, err
	}
	p := &Project{
		Name:        spec.Name,
		Spec:        spec,
		Root:        b.Root,
		StateDir:    b.StateDir,
		Tracker:     b.Tracker,
		Repo:        b.Repo,
		Agent:       b.Agent,
		AgentName:   b.AgentName,
		Labels:      cfg.LabelsFor(spec),
		trackerConn: spec.Tracker.Connection,
		repoConn:    spec.Repo.Connection,
		fingerprint: fp,
		branchTmpl:  b.BranchTemplate,
		branchRe:    b.branchRe,
		base:        spec.BaseBranch,
		hub:         s,
		running:     map[string]adapter.Turn{},
		checkTimers: map[string]*time.Timer{},
	}
	p.Store = store.New(b.StateDir, s.changed)
	p.Jobs = loadJobs(p)
	if events := p.Jobs.Events(); len(events) > 0 {
		subscribed := false
		for _, h := range []any{p.Tracker, p.Repo} {
			if eh, ok := h.(adapter.EventHooked); ok {
				eh.WantEvents(events)
				subscribed = true
			}
		}
		if !subscribed {
			p.logf("jobs run on %s, but neither %s nor %s sends events", strings.Join(events, ", "), p.Tracker.Describe(), p.Repo.Describe())
		}
	}
	return p, nil
}

func (s *Supervisor) openSinks(cfg *config.Config) []adapter.Telemetry {
	var out []adapter.Telemetry
	for _, name := range cfg.Settings.Telemetry {
		conn, err := s.conns.Get(name)
		if err != nil {
			logx.Default.Printf("telemetry %s: %s", name, logx.ErrText(err))
			continue
		}
		if t, ok := conn.(adapter.Telemetry); ok {
			out = append(out, t)
		}
	}
	return out
}

func gitRemote(root string) string {
	out, err := exec.Command("git", "-C", root, "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// readDotEnv reads KEY=value lines, ignoring comments, `export` and quotes.
func readDotEnv(file string) map[string]string {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		out[strings.TrimSpace(k)] = v
	}
	return out
}
