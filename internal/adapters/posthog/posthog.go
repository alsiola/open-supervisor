// Package posthog sends the supervisor's metrics to PostHog: one event per turn (tokens, cost, duration, how
// it ended) and one whenever an agent's usage windows change. It posts to PostHog's capture endpoint directly,
// so nothing else is installed.
package posthog

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/alsiola/open-supervisor/internal/adapter"
)

func init() {
	adapter.Register(adapter.Spec{
		Type:        "posthog",
		Title:       "PostHog",
		Description: "Sends an agent_turn event per turn and an agent_usage event when usage changes.",
		Roles:       []adapter.Role{adapter.RoleTelemetry},
		Fields: []adapter.Field{
			{Key: "token", Label: "Project API key", Help: "The project's public API key (phc_…), from Project settings.", Type: adapter.Secret, Required: true},
			{Key: "host", Label: "Host", Help: "Your PostHog ingestion host.", Type: adapter.Select, Options: []string{"https://us.i.posthog.com", "https://eu.i.posthog.com"}, Default: "https://us.i.posthog.com"},
			{Key: "distinctId", Label: "Distinct id", Help: "Who events are from; defaults to open-supervisor@<hostname>.", Type: adapter.Text},
		},
		New: func(v adapter.Values, env adapter.Env) (adapter.Connection, error) {
			token, err := env.Secret(v.Get("token", ""))
			if err != nil {
				return nil, err
			}
			host, _ := os.Hostname()
			id := v.Get("distinctId", "open-supervisor@"+host)
			client := env.HTTP
			if client == nil {
				client = http.DefaultClient
			}
			return &Sink{token: token, distinctID: id, api: &adapter.JSONClient{HTTP: client, BaseURL: strings.TrimRight(v.Get("host", "https://us.i.posthog.com"), "/")}}, nil
		},
	})
}

// Sink is a PostHog project.
type Sink struct {
	token, distinctID string
	api               *adapter.JSONClient
}

// Check says where events go; PostHog's capture endpoint can't be asked about a key without sending one.
func (s *Sink) Check(ctx context.Context) (string, error) {
	if !strings.HasPrefix(s.token, "phc_") {
		return "", fmt.Errorf("that doesn't look like a project API key (they start phc_)")
	}
	return "sending to " + s.api.BaseURL, nil
}

// Capture sends one event.
func (s *Sink) Capture(ctx context.Context, e adapter.TelemetryEvent) error {
	props := map[string]any{"$lib": "open-supervisor"}
	for k, v := range e.Props {
		props[k] = v
	}
	_, err := s.api.Do(ctx, http.MethodPost, "/i/v0/e/", map[string]any{
		"api_key":     s.token,
		"event":       e.Name,
		"distinct_id": s.distinctID,
		"timestamp":   e.Time,
		"properties":  props,
	}, nil)
	return err
}

// Close has nothing to flush: every event is sent as it comes.
func (s *Sink) Close(ctx context.Context) error { return nil }
