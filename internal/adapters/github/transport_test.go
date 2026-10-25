package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alsiola/open-supervisor/internal/adapter"
)

func TestUnchangedPollsAreConditional(t *testing.T) {
	var full, notModified int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		etag := `"v1-` + r.Header.Get("Authorization") + `"`
		if r.Header.Get("If-None-Match") == etag {
			atomic.AddInt32(&notModified, 1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		atomic.AddInt32(&full, 1)
		w.Header().Set("ETag", etag)
		fmt.Fprint(w, `{"number":12,"title":"Fix the thing","state":"open"}`)
	}))
	t.Cleanup(srv.Close)
	c, err := newConn(adapter.Values{"auth": "token", "token": "tok", "apiUrl": srv.URL}, adapter.Env{HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	tr, err := c.Tracker(adapter.Values{"repo": "octo/widgets"}, adapter.ProjectInfo{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		is, err := tr.Issue(context.Background(), "12")
		if err != nil {
			t.Fatal(err)
		}
		if is.Title != "Fix the thing" {
			t.Fatalf("poll %d read %+v", i, is)
		}
	}
	if full != 1 || notModified != 2 {
		t.Fatalf("full answers %d, 304s %d; want 1 and 2", full, notModified)
	}

	// Another token has its own cache entry: its first call is a full one.
	c.read = staticToken("other")
	if _, err := tr.Issue(context.Background(), "12"); err != nil {
		t.Fatal(err)
	}
	if full != 2 {
		t.Fatalf("another token was sent a cached ETag (%d full answers)", full)
	}
}

func TestRateLimitPausesUntilReset(t *testing.T) {
	now := time.Date(2026, 10, 24, 11, 0, 0, 0, time.UTC)
	reset := now.Add(35 * time.Minute)
	var calls, limited int32
	limited = 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if atomic.LoadInt32(&limited) == 1 {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, "{\n  \"message\": \"API rate limit exceeded for user ID 1.\",\n  \"documentation_url\": \"https://docs.github.com\"\n}")
			return
		}
		fmt.Fprint(w, `{"number":12,"title":"t","state":"open"}`)
	}))
	t.Cleanup(srv.Close)
	var mu sync.Mutex
	var lines []string
	logf := func(format string, args ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	c, err := newConn(adapter.Values{"auth": "token", "token": "tok", "apiUrl": srv.URL}, adapter.Env{HTTP: srv.Client(), Log: logf})
	if err != nil {
		t.Fatal(err)
	}
	clock := now
	c.tr.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	tr, _ := c.Tracker(adapter.Values{"repo": "octo/widgets"}, adapter.ProjectInfo{})

	for i := 0; i < 5; i++ {
		_, err := tr.Issue(context.Background(), "12")
		var paused *adapter.PausedError
		if !errors.As(err, &paused) || !adapter.IsPaused(err) {
			t.Fatalf("call %d: %v, want a PausedError", i, err)
		}
		if !paused.Until.Equal(reset) {
			t.Fatalf("paused until %s, want %s", paused.Until, reset)
		}
	}
	if calls != 1 {
		t.Fatalf("%d requests reached GitHub during the pause, want 1", calls)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "GitHub rate limit reached; pausing until "+reset.Local().Format("15:04")) ||
		!strings.Contains(lines[0], "API rate limit exceeded for user ID 1.") {
		t.Fatalf("log %q, want one pause line", lines)
	}

	mu.Lock()
	clock = reset.Add(-time.Second)
	mu.Unlock()
	if _, err := tr.Issue(context.Background(), "12"); !adapter.IsPaused(err) || calls != 1 {
		t.Fatalf("a second before the reset: %v, %d requests", err, calls)
	}

	atomic.StoreInt32(&limited, 0)
	mu.Lock()
	clock = reset
	mu.Unlock()
	if _, err := tr.Issue(context.Background(), "12"); err != nil {
		t.Fatalf("after the reset: %v", err)
	}
	if calls != 2 || len(lines) != 1 {
		t.Fatalf("after the reset: %d requests, log %q", calls, lines)
	}
}

func TestSecondaryRateLimitWaitsForRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 24, 11, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "90")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"message":"You have exceeded a secondary rate limit."}`)
	}))
	t.Cleanup(srv.Close)
	c, err := newConn(adapter.Values{"auth": "token", "token": "tok", "apiUrl": srv.URL}, adapter.Env{HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	c.tr.now = func() time.Time { return now }
	tr, _ := c.Tracker(adapter.Values{"repo": "octo/widgets"}, adapter.ProjectInfo{})
	_, err = tr.Issue(context.Background(), "12")
	var paused *adapter.PausedError
	if !errors.As(err, &paused) || !paused.Until.Equal(now.Add(90*time.Second)) {
		t.Fatalf("%v, want a pause of 90s", err)
	}
}

func TestForbiddenWithoutRateLimitIsAnOrdinaryError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "4000")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "{\n  \"message\": \"Resource not accessible by integration\",\n  \"status\": \"403\"\n}")
	}))
	t.Cleanup(srv.Close)
	c, err := newConn(adapter.Values{"auth": "token", "token": "tok", "apiUrl": srv.URL}, adapter.Env{HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	tr, _ := c.Tracker(adapter.Values{"repo": "octo/widgets"}, adapter.ProjectInfo{})
	_, err = tr.Issue(context.Background(), "12")
	if adapter.IsPaused(err) || !adapter.IsStatus(err, http.StatusForbidden) {
		t.Fatalf("%v, want a plain 403", err)
	}
	if got := err.Error(); strings.Contains(got, "\n") || !strings.HasSuffix(got, ": 403 Resource not accessible by integration") {
		t.Fatalf("error text %q, want one line ending in GitHub's message", got)
	}
}

func TestRateLimitedReadsGoAsTheBot(t *testing.T) {
	var asBot int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer bot" {
			atomic.AddInt32(&asBot, 1)
			fmt.Fprint(w, `{"number":12,"title":"t","state":"open"}`)
			return
		}
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"API rate limit exceeded for user ID 1."}`)
	}))
	t.Cleanup(srv.Close)
	c, err := newConn(adapter.Values{"auth": "token", "token": "tok", "apiUrl": srv.URL}, adapter.Env{HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	tr, _ := c.Tracker(adapter.Values{"repo": "octo/widgets"}, adapter.ProjectInfo{})
	if _, err := tr.Issue(context.Background(), "12"); !adapter.IsPaused(err) {
		t.Fatalf("without a bot: %v, want a PausedError", err)
	}

	c.write = staticToken("bot")
	for i := 0; i < 2; i++ {
		if is, err := tr.Issue(context.Background(), "12"); err != nil || is.Title != "t" {
			t.Fatalf("read %d with a bot: %+v, %v", i, is, err)
		}
	}
	if asBot != 2 {
		t.Fatalf("%d reads went as the bot, want 2", asBot)
	}
}
