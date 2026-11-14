package adapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestAPIErrorIsOneLine(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"{\n  \"message\": \"API rate limit exceeded for user ID 1.\",\n  \"documentation_url\": \"x\"\n}", "GET u: 403 API rate limit exceeded for user ID 1."},
		{"<html>\n<body>Bad gateway</body>\n</html>", "GET u: 403 <html> <body>Bad gateway</body> </html>"},
		{`{"errors":["no"]}`, `GET u: 403 {"errors":["no"]}`},
	} {
		if got := (&APIError{Method: "GET", URL: "u", Status: 403, Body: tc.body}).Error(); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
}

func TestMayHaveLanded(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&APIError{Status: 502}, true},
		{fmt.Errorf("creating: %w", &APIError{Status: 504}), true},
		{&APIError{Status: 422}, false},
		{context.DeadlineExceeded, true},
		{io.ErrUnexpectedEOF, true},
		{errors.New("bad credentials"), false},
		{&PausedError{Service: "GitHub"}, false},
	} {
		if got := MayHaveLanded(tc.err); got != tc.want {
			t.Errorf("%v: got %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestJSONClientRetriesWhatIsSafeToResend(t *testing.T) {
	for _, tc := range []struct {
		name, method string
		status       int
		retry        []time.Duration
		want         int32
	}{
		{"a read after a 502", http.MethodGet, http.StatusBadGateway, []time.Duration{0, 0}, 3},
		{"an edit after a 500", http.MethodPatch, http.StatusInternalServerError, []time.Duration{0, 0}, 3},
		{"a post after a 500 may have landed", http.MethodPost, http.StatusInternalServerError, []time.Duration{0, 0}, 1},
		{"a 404 is final", http.MethodGet, http.StatusNotFound, []time.Duration{0, 0}, 1},
		{"no retries set", http.MethodGet, http.StatusBadGateway, nil, 1},
	} {
		var calls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			w.WriteHeader(tc.status)
		}))
		c := &JSONClient{HTTP: srv.Client(), BaseURL: srv.URL, Retry: tc.retry}
		if _, err := c.Do(context.Background(), tc.method, "/x", map[string]string{"a": "b"}, nil); !IsStatus(err, tc.status) {
			t.Errorf("%s: err %v", tc.name, err)
		}
		if calls != tc.want {
			t.Errorf("%s: %d calls, want %d", tc.name, calls, tc.want)
		}
		srv.Close()
	}
}

func TestJSONClientRetryRecoversAndResendsTheBody(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"a":"b"}` {
			t.Errorf("body %q", body)
		}
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()
	c := &JSONClient{HTTP: srv.Client(), BaseURL: srv.URL, Retry: []time.Duration{0}}
	var out struct{ OK bool }
	if _, err := c.Do(context.Background(), http.MethodPut, "/x", map[string]string{"a": "b"}, &out); err != nil || !out.OK {
		t.Fatalf("got %v %v", out, err)
	}
}

func TestJSONClientResendsAPostThatWasNeverSent(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens, so every dial is refused
	var dials int32
	tr := &http.Transport{DialContext: func(ctx context.Context, network, a string) (net.Conn, error) {
		atomic.AddInt32(&dials, 1)
		return (&net.Dialer{}).DialContext(ctx, network, a)
	}}
	c := &JSONClient{HTTP: &http.Client{Transport: tr}, BaseURL: "http://" + addr, Retry: []time.Duration{0, 0}}
	if _, err := c.Do(context.Background(), http.MethodPost, "/x", nil, nil); err == nil || !NotSent(err) {
		t.Fatalf("err %v, want a refused dial", err)
	}
	if dials != 3 {
		t.Errorf("%d dials, want 3", dials)
	}
}

func TestJSONClientRetryStopsWithTheContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	c := &JSONClient{HTTP: srv.Client(), BaseURL: srv.URL, Retry: []time.Duration{time.Hour}}
	start := time.Now()
	if _, err := c.Do(ctx, http.MethodGet, "/x", nil, nil); !IsStatus(err, http.StatusBadGateway) {
		t.Errorf("err %v, want the 502", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("waited %v past the context", time.Since(start))
	}
}

func TestTransient(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&APIError{Status: 500}, true},
		{&APIError{Status: 422}, false},
		{context.DeadlineExceeded, true},
		{io.EOF, true},
		{&net.OpError{Op: "dial", Err: errors.New("refused")}, true},
		{&PausedError{Service: "GitHub"}, false},
		{errors.New("bad credentials"), false},
		{nil, false},
	} {
		if got := Transient(tc.err); got != tc.want {
			t.Errorf("%v: got %v, want %v", tc.err, got, tc.want)
		}
	}
}
