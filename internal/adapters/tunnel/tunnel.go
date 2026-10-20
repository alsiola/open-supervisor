// Package tunnel exposes the webhook port to the internet so GitHub, Bitbucket and Jira can reach it: ngrok,
// a Cloudflare quick tunnel, or a URL you already route to this machine. A tunnel process that dies is restarted
// with a growing wait; each new public URL is reported so the supervisor re-points the hooks.
package tunnel

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/alsiola/open-supervisor/internal/adapter"
)

func init() {
	adapter.Register(adapter.Spec{
		Type:        "ngrok",
		Title:       "ngrok",
		Description: "Runs `ngrok http <port>`. A reserved domain keeps the webhook URL stable across restarts.",
		Roles:       []adapter.Role{adapter.RoleTunnel},
		Fields: []adapter.Field{
			{Key: "binary", Label: "ngrok binary", Type: adapter.Text, Default: "ngrok"},
			{Key: "domain", Label: "Reserved domain", Help: "Optional, e.g. my-supervisor.ngrok.app; without one the URL changes on every start.", Type: adapter.Text},
		},
		New: func(v adapter.Values, env adapter.Env) (adapter.Connection, error) {
			return &Ngrok{binary: v.Get("binary", "ngrok"), domain: v.Get("domain", ""), log: env.Log}, nil
		},
	})
	adapter.Register(adapter.Spec{
		Type:        "cloudflared",
		Title:       "Cloudflare Tunnel",
		Description: "Runs a Cloudflare quick tunnel (`cloudflared tunnel --url`), or a named tunnel you have set up.",
		Roles:       []adapter.Role{adapter.RoleTunnel},
		Fields: []adapter.Field{
			{Key: "binary", Label: "cloudflared binary", Type: adapter.Text, Default: "cloudflared"},
			{Key: "tunnel", Label: "Named tunnel", Help: "Optional: a tunnel already routed to this port; leave empty for a quick tunnel.", Type: adapter.Text},
			{Key: "hostname", Label: "Public hostname", Help: "The named tunnel's hostname (https://hooks.example.com).", Type: adapter.Text},
		},
		New: func(v adapter.Values, env adapter.Env) (adapter.Connection, error) {
			return &Cloudflared{binary: v.Get("binary", "cloudflared"), tunnel: v.Get("tunnel", ""), hostname: v.Get("hostname", ""), log: env.Log}, nil
		},
	})
	adapter.Register(adapter.Spec{
		Type:        "static",
		Title:       "Public URL",
		Description: "A URL you already route to this machine's webhook port (a reverse proxy, Tailscale Funnel).",
		Roles:       []adapter.Role{adapter.RoleTunnel},
		Fields: []adapter.Field{
			{Key: "url", Label: "Public URL", Help: "https://hooks.example.com, forwarding to 127.0.0.1:<port>.", Type: adapter.Text, Required: true},
		},
		New: func(v adapter.Values, env adapter.Env) (adapter.Connection, error) {
			return &Static{url: strings.TrimRight(v.Get("url", ""), "/")}, nil
		},
	})
}

// supervise runs a tunnel process until ctx ends, restarting it with a growing wait, and passes each line of
// its output to line, which returns a public URL when it sees one.
func supervise(ctx context.Context, name string, start func() *exec.Cmd, line func(string) string, log func(string, ...any), urls chan<- string) error {
	backoff := 5 * time.Second
	for {
		cmd := start()
		out, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		cmd.Stderr = cmd.Stdout
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("couldn't start %s: %w", name, err)
		}
		go func() {
			<-ctx.Done()
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		}()
		scan(out, func(l string) {
			if u := line(l); u != "" {
				backoff = 5 * time.Second
				urls <- u
			}
		})
		_ = cmd.Wait()
		urls <- ""
		if ctx.Err() != nil {
			return nil
		}
		log("tunnel: %s exited; restarting in %s", name, backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Minute)
	}
}

func scan(r io.Reader, each func(string)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		each(sc.Text())
	}
}

// Ngrok is an ngrok agent.
type Ngrok struct {
	binary, domain string
	log            func(string, ...any)
}

// Check runs `ngrok version`.
func (n *Ngrok) Check(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, n.binary, "version").Output()
	if err != nil {
		return "", fmt.Errorf("%s version: %w (install it from ngrok.com and run `ngrok config add-authtoken`)", n.binary, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Run keeps `ngrok http <port>` up.
func (n *Ngrok) Run(ctx context.Context, port int, urls chan<- string) error {
	return supervise(ctx, "ngrok", func() *exec.Cmd {
		args := []string{"http", fmt.Sprint(port), "--log", "stdout", "--log-format", "json"}
		if n.domain != "" {
			args = append(args, "--url", n.domain)
		}
		return exec.Command(n.binary, args...)
	}, func(l string) string {
		var e struct {
			Msg string `json:"msg"`
			URL string `json:"url"`
			Lvl string `json:"lvl"`
			Err string `json:"err"`
		}
		if json.Unmarshal([]byte(l), &e) != nil {
			return ""
		}
		if e.Msg == "started tunnel" && e.URL != "" {
			return e.URL
		}
		if e.Lvl == "eror" || e.Lvl == "error" {
			msg := e.Err
			if msg == "" {
				msg = e.Msg
			}
			n.log("tunnel: %s", msg)
		}
		return ""
	}, n.log, urls)
}

// Cloudflared is a Cloudflare tunnel.
type Cloudflared struct {
	binary, tunnel, hostname string
	log                      func(string, ...any)
}

var trycloudflare = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

// Check runs `cloudflared --version`.
func (c *Cloudflared) Check(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, c.binary, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w (install cloudflared from Cloudflare)", c.binary, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Run keeps the tunnel up.
func (c *Cloudflared) Run(ctx context.Context, port int, urls chan<- string) error {
	return supervise(ctx, "cloudflared", func() *exec.Cmd {
		if c.tunnel != "" {
			return exec.Command(c.binary, "tunnel", "run", "--url", fmt.Sprintf("http://127.0.0.1:%d", port), c.tunnel)
		}
		return exec.Command(c.binary, "tunnel", "--no-autoupdate", "--url", fmt.Sprintf("http://127.0.0.1:%d", port))
	}, func(l string) string {
		if c.tunnel != "" {
			if strings.Contains(l, "Registered tunnel connection") && c.hostname != "" {
				return strings.TrimRight(c.hostname, "/")
			}
			return ""
		}
		return trycloudflare.FindString(l)
	}, c.log, urls)
}

// Static is a URL already routed to the port.
type Static struct{ url string }

// Check says what the URL is.
func (s *Static) Check(ctx context.Context) (string, error) {
	if !strings.HasPrefix(s.url, "https://") && !strings.HasPrefix(s.url, "http://") {
		return "", fmt.Errorf("%q isn't a URL", s.url)
	}
	return "webhooks go to " + s.url, nil
}

// Run reports the URL and waits.
func (s *Static) Run(ctx context.Context, port int, urls chan<- string) error {
	urls <- s.url
	<-ctx.Done()
	return nil
}
