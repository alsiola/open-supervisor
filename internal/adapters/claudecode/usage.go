package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/alsiola/open-supervisor/internal/core"
)

// The subscription's rate-limit windows: the 5-hour and 7-day percentages /usage shows in a session, read from
// the OAuth usage endpoint the CLI itself uses, with the login token Claude Code keeps (the macOS Keychain, or
// ~/.claude/.credentials.json elsewhere). The endpoint isn't documented, so when it changes the TUI says
// "usage unknown" and the log says why.

const usageURL = "https://api.anthropic.com/api/oauth/usage"

func loginToken(ctx context.Context) (string, error) {
	var raw []byte
	if runtime.GOOS == "darwin" {
		out, err := exec.CommandContext(ctx, "security", "find-generic-password", "-s", "Claude Code-credentials", "-w").Output()
		if err == nil {
			raw = out
		}
	}
	if raw == nil {
		data, err := os.ReadFile(filepath.Join(claudeDir(), ".credentials.json"))
		if err != nil {
			return "", fmt.Errorf("no Claude login found (sign in with `claude` first): %w", err)
		}
		raw = data
	}
	var creds struct {
		ClaudeAIOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &creds); err != nil || creds.ClaudeAIOauth.AccessToken == "" {
		return "", errors.New("no Claude login token found (an API-key login has no subscription windows)")
	}
	return creds.ClaudeAIOauth.AccessToken, nil
}

// Usage reads the subscription's windows; nil when usage reading is off.
func (a *Agent) Usage(ctx context.Context) (*core.Usage, error) {
	if !a.usage {
		return nil, nil
	}
	token, err := loginToken(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", "open-supervisor")
	client := a.env.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("usage endpoint answered %d", res.StatusCode)
	}
	type window struct {
		Utilization *float64 `json:"utilization"`
		ResetsAt    *string  `json:"resets_at"`
	}
	var body struct {
		FiveHour *window `json:"five_hour"`
		SevenDay *window `json:"seven_day"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, err
	}
	u := &core.Usage{FetchedAt: time.Now().UTC()}
	add := func(name string, w *window) {
		if w == nil || w.Utilization == nil || w.ResetsAt == nil {
			return
		}
		at, _ := time.Parse(time.RFC3339Nano, *w.ResetsAt)
		u.Windows = append(u.Windows, core.UsageWindow{Name: name, Used: *w.Utilization, ResetsAt: at})
	}
	add("5h", body.FiveHour)
	add("7d", body.SevenDay)
	return u, nil
}
