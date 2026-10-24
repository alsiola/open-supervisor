package github

import (
	"bytes"
	"container/list"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alsiola/open-supervisor/internal/adapter"
)

// cacheSize is how many GET answers the transport keeps for conditional requests.
const cacheSize = 4096

// transport sits under every call the connection makes. It sends GETs as conditional requests, so a poll of
// something unchanged comes back 304 and doesn't count against the rate limit, and once GitHub says a token is
// rate limited it refuses that token's calls until the limit resets, without making them. Only API answers are
// cached, never files downloaded from elsewhere.
type transport struct {
	base    http.RoundTripper
	apiHost string
	now     func() time.Time
	log     func(format string, args ...any)

	mu     sync.Mutex
	order  *list.List               // of *cached, most recently used first
	cache  map[string]*list.Element // token + URL → its element in order
	paused map[string]time.Time     // token → when its calls may go again
}

type cached struct {
	key    string
	etag   string
	status int
	header http.Header
	body   []byte
}

func newTransport(base http.RoundTripper, apiHost string, log func(string, ...any)) *transport {
	if base == nil {
		base = http.DefaultTransport
	}
	if log == nil {
		log = func(string, ...any) {}
	}
	return &transport{base: base, apiHost: apiHost, now: time.Now, log: log, order: list.New(), cache: map[string]*list.Element{}, paused: map[string]time.Time{}}
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	// The same token goes as "Bearer <t>" to the API and "token <t>" for attachments.
	auth := req.Header.Get("Authorization")
	tok := auth
	if _, t, ok := strings.Cut(auth, " "); ok {
		tok = t
	}
	if until, ok := t.pausedUntil(tok); ok {
		return nil, &adapter.PausedError{Service: "GitHub", Until: until}
	}
	key := tok + " " + req.URL.String()
	conditional := req.Method == http.MethodGet && req.URL.Host == t.apiHost && req.Header.Get("If-None-Match") == ""
	var hit *cached
	if conditional {
		if hit = t.lookup(key); hit != nil {
			req = req.Clone(req.Context())
			req.Header.Set("If-None-Match", hit.etag)
		}
	}
	res, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if until, msg, limited := t.rateLimited(res); limited {
		res.Body.Close()
		t.pause(tok, until, msg)
		return nil, &adapter.PausedError{Service: "GitHub", Until: until}
	}
	if !conditional {
		return res, nil
	}
	if res.StatusCode == http.StatusNotModified && hit != nil {
		res.Body.Close()
		return hit.response(req), nil
	}
	etag := res.Header.Get("ETag")
	if res.StatusCode != http.StatusOK || etag == "" {
		return res, nil
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	res.Body.Close()
	if err != nil {
		return nil, err
	}
	t.store(&cached{key: key, etag: etag, status: res.StatusCode, header: res.Header.Clone(), body: body})
	res.Body = io.NopCloser(bytes.NewReader(body))
	return res, nil
}

// response is the cached answer, served for a 304.
func (c *cached) response(req *http.Request) *http.Response {
	return &http.Response{
		Status:        strconv.Itoa(c.status) + " " + http.StatusText(c.status),
		StatusCode:    c.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        c.header.Clone(),
		Body:          io.NopCloser(bytes.NewReader(c.body)),
		ContentLength: int64(len(c.body)),
		Request:       req,
	}
}

func (t *transport) lookup(key string) *cached {
	t.mu.Lock()
	defer t.mu.Unlock()
	el, ok := t.cache[key]
	if !ok {
		return nil
	}
	t.order.MoveToFront(el)
	return el.Value.(*cached)
}

func (t *transport) store(c *cached) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if el, ok := t.cache[c.key]; ok {
		el.Value = c
		t.order.MoveToFront(el)
		return
	}
	t.cache[c.key] = t.order.PushFront(c)
	for t.order.Len() > cacheSize {
		last := t.order.Back()
		t.order.Remove(last)
		delete(t.cache, last.Value.(*cached).key)
	}
}

func (t *transport) pausedUntil(tok string) (time.Time, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	until, ok := t.paused[tok]
	if !ok {
		return time.Time{}, false
	}
	if !t.now().Before(until) {
		delete(t.paused, tok)
		return time.Time{}, false
	}
	return until, true
}

// pause stops the token's calls until then, logging once when a pause starts.
func (t *transport) pause(tok string, until time.Time, msg string) {
	t.mu.Lock()
	prev, already := t.paused[tok]
	already = already && t.now().Before(prev)
	if !already || until.After(prev) {
		t.paused[tok] = until
	}
	t.mu.Unlock()
	if already {
		return
	}
	line := "GitHub rate limit reached; pausing until " + until.Local().Format("15:04")
	if msg != "" {
		line += " (" + msg + ")"
	}
	t.log("%s", line)
}

// rateLimited says whether the answer is GitHub's primary or secondary rate limit, and when calls may go again.
func (t *transport) rateLimited(res *http.Response) (time.Time, string, bool) {
	if res.StatusCode != http.StatusForbidden && res.StatusCode != http.StatusTooManyRequests {
		return time.Time{}, "", false
	}
	now := t.now()
	var until time.Time
	if s := res.Header.Get("Retry-After"); s != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			until = now.Add(time.Duration(secs) * time.Second)
		}
	}
	if until.IsZero() && res.Header.Get("X-Ratelimit-Remaining") == "0" {
		if secs, err := strconv.ParseInt(res.Header.Get("X-Ratelimit-Reset"), 10, 64); err == nil {
			until = time.Unix(secs, 0)
		}
	}
	// A 403 that is neither (no access to the repo, say) is an ordinary error, so its body stays for the caller.
	data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	res.Body.Close()
	res.Body = io.NopCloser(bytes.NewReader(data))
	var body struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(data, &body)
	msg := strings.Join(strings.Fields(body.Message), " ")
	if until.IsZero() && strings.Contains(strings.ToLower(msg), "rate limit") {
		// GitHub's advice when a secondary limit says nothing about how long: wait at least a minute.
		until = now.Add(time.Minute)
	}
	if until.IsZero() {
		return time.Time{}, "", false
	}
	if !until.After(now) {
		until = now.Add(time.Second)
	}
	return until, msg, true
}
