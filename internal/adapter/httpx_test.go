package adapter

import "testing"

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
