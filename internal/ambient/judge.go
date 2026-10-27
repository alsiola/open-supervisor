// Package ambient is the coordination experiment's machinery: asking a decisions model whether what one agent
// posted could bear on another agent's issue, remembering what was asked and shared so nothing is asked or
// shared twice, and the comment a share is posted as. The engine decides when to ask and where to post.
package ambient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/alsiola/open-supervisor/internal/adapter"
)

// Ask is one question for the model: could Message bear on Issue?
type Ask struct {
	// Message is what an agent posted, and Source where (which issue or PR, and what that issue is about).
	Message, Source string
	// Issue is the issue it may bear on (title and description), and Progress where its agent has got to.
	Issue, Progress string
}

// Verdict is the model's answer.
type Verdict struct {
	// P is the probability that the message could materially affect the issue's work.
	P float64
	// Model is the model that answered, with its version.
	Model        string
	InputTokens  int
	OutputTokens int
	// Cost is what OpenRouter charged, in US dollars.
	Cost float64
	// Attempts is how many requests it took.
	Attempts int
}

// Judge answers Asks.
type Judge interface {
	Judge(ctx context.Context, a Ask) (Verdict, error)
}

// Question is what the model is asked about every pair, over the state an Ask becomes.
const Question = "`message` contains information that could materially affect the investigation, implementation or " +
	"decisions of the work on `issue`: evidence, a warning, a constraint, a finding or a decision its agent would want " +
	"and doesn't have from `issue` or `progress`, such that knowing it could change their understanding or approach. " +
	"Being about the same subsystem or using the same words is not enough; a message about an apparently unrelated part " +
	"can count when it reveals a cause, constraint or problem the issue shares. Routine status (started, pushed, " +
	"checks passed, waiting) never counts."

// OpenRouter asks a decisions model through OpenRouter's decisions endpoint.
type OpenRouter struct {
	api   *adapter.JSONClient
	model string
	// Backoff is the wait before the second attempt, doubled before the third.
	Backoff time.Duration
}

// NewOpenRouter is a Judge on OpenRouter; baseURL is empty for OpenRouter itself.
func NewOpenRouter(key, model, baseURL string, client *http.Client) *OpenRouter {
	if baseURL == "" {
		baseURL = "https://openrouter.ai/api/alpha"
	}
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	return &OpenRouter{
		model:   model,
		Backoff: 2 * time.Second,
		api: &adapter.JSONClient{HTTP: client, BaseURL: baseURL, Auth: func(r *http.Request) error {
			r.Header.Set("Authorization", "Bearer "+key)
			return nil
		}},
	}
}

type decisionAnswer struct {
	Model   string `json:"model"`
	Answers struct {
		Useful struct {
			Noul *float64 `json:"noul"`
		} `json:"useful"`
	} `json:"answers"`
	Usage struct {
		InputTokens  int     `json:"input_tokens"`
		OutputTokens int     `json:"output_tokens"`
		Cost         float64 `json:"cost"`
	} `json:"usage"`
}

// Judge asks once, trying again twice when OpenRouter is busy, failing or out of reach.
func (o *OpenRouter) Judge(ctx context.Context, a Ask) (Verdict, error) {
	req := map[string]any{
		"model": o.model,
		"state": map[string]string{"message": a.Message, "source": a.Source, "issue": a.Issue, "progress": a.Progress},
		"questions": map[string]any{
			"useful": map[string]string{"type": "noul", "instructions": Question},
		},
	}
	var err error
	for attempt := 1; ; attempt++ {
		var ans decisionAnswer
		_, err = o.api.Do(ctx, http.MethodPost, "decisions", req, &ans)
		if err == nil {
			if ans.Answers.Useful.Noul == nil {
				return Verdict{}, fmt.Errorf("%s answered without a probability", o.model)
			}
			return Verdict{P: *ans.Answers.Useful.Noul, Model: ans.Model, InputTokens: ans.Usage.InputTokens, OutputTokens: ans.Usage.OutputTokens, Cost: ans.Usage.Cost, Attempts: attempt}, nil
		}
		if attempt >= 3 || !retryable(err) || ctx.Err() != nil {
			return Verdict{Attempts: attempt}, err
		}
		select {
		case <-ctx.Done():
			return Verdict{Attempts: attempt}, ctx.Err()
		case <-time.After(o.Backoff << (attempt - 1)):
		}
	}
}

// retryable: a rate limit, a server error or no answer at all; a bad request or key won't get better.
func retryable(err error) bool {
	var e *adapter.APIError
	if errors.As(err, &e) {
		return e.Status == http.StatusTooManyRequests || e.Status >= 500
	}
	return true
}
