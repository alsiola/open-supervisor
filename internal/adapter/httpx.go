package adapter

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// APIError is a service answering a request with an error status.
type APIError struct {
	Method, URL string
	Status      int
	Body        string
}

// Error is one line: the body's JSON message when it has one (GitHub's errors do),
// else the body with its whitespace folded.
func (e *APIError) Error() string {
	body := strings.Join(strings.Fields(e.Body), " ")
	var msg struct {
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(e.Body), &msg) == nil && strings.TrimSpace(msg.Message) != "" {
		body = strings.Join(strings.Fields(msg.Message), " ")
	}
	if len(body) > 300 {
		body = body[:300] + "…"
	}
	return fmt.Sprintf("%s %s: %d %s", e.Method, e.URL, e.Status, body)
}

// IsStatus reports whether err is an APIError with that status.
func IsStatus(err error, status int) bool {
	var e *APIError
	return errors.As(err, &e) && e.Status == status
}

// PausedError is a call an adapter refused without making it, because the service asked it to wait (a rate
// limit). The adapter logs the pause once; callers can skip the work quietly.
type PausedError struct {
	Service string
	Until   time.Time
}

func (e *PausedError) Error() string {
	return fmt.Sprintf("%s rate limit reached; paused until %s", e.Service, e.Until.Local().Format("15:04"))
}

// IsPaused reports whether err is a call refused during a PausedError's pause.
func IsPaused(err error) bool {
	var e *PausedError
	return errors.As(err, &e)
}

// JSONClient makes JSON API calls with one way of authenticating.
type JSONClient struct {
	HTTP    *http.Client
	BaseURL string
	// Auth sets the request's credentials.
	Auth func(r *http.Request) error
	// Header is added to every request.
	Header http.Header
}

// Do sends body (as JSON, unless it is nil) and decodes the answer into out (unless it is nil). It returns the
// response so callers can read headers, such as the next page's link.
func (c *JSONClient) Do(ctx context.Context, method, path string, body, out any) (*http.Response, error) {
	url := path
	if !strings.HasPrefix(path, "http://") && !strings.HasPrefix(path, "https://") {
		url = strings.TrimRight(c.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, vs := range c.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if c.Auth != nil {
		if err := c.Auth(req); err != nil {
			return nil, err
		}
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return res, err
	}
	if res.StatusCode >= 300 {
		return res, &APIError{Method: method, URL: url, Status: res.StatusCode, Body: string(data)}
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return res, fmt.Errorf("%s %s: decoding the answer: %w", method, url, err)
		}
	}
	return res, nil
}

// VerifyHMAC checks a "sha256=<hex>" signature over body, the form GitHub, Bitbucket and Jira all use.
func VerifyHMAC(secret string, body []byte, signature string) bool {
	sig, ok := strings.CutPrefix(signature, "sha256=")
	if !ok {
		return false
	}
	given, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), given)
}

// Sign makes the signature VerifyHMAC accepts, for tests.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
