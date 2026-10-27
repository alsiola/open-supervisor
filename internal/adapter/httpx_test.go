package adapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
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
