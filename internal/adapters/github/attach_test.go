package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAttachStartsTheBranchThenCommits(t *testing.T) {
	var calls []string
	var put map[string]string
	_, rp := bound(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/octo/widgets/branches/"+attachBranch:
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/octo/widgets/git/commits":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if parents, _ := body["parents"].([]any); body["tree"] != "t1" || parents == nil || len(parents) != 0 {
				t.Errorf("commit: %v", body)
			}
			w.Write([]byte(`{"sha":"c1"}`))
		case r.Method == http.MethodPost:
			w.Write([]byte(`{"sha":"t1"}`))
		case r.Method == http.MethodPut:
			json.NewDecoder(r.Body).Decode(&put)
			w.Write([]byte(`{}`))
		}
	}))
	link, err := rp.Attach(context.Background(), "38", "my shot (1).png", []byte("png"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /repos/octo/widgets/branches/" + attachBranch, "POST /repos/octo/widgets/git/trees", "POST /repos/octo/widgets/git/commits", "POST /repos/octo/widgets/git/refs"}
	if len(calls) != 5 || strings.Join(calls[:4], ",") != strings.Join(want, ",") || !strings.HasPrefix(calls[4], "PUT /repos/octo/widgets/contents/38/") || !strings.HasSuffix(calls[4], "-my-shot-1-.png") {
		t.Fatalf("calls: %v", calls)
	}
	if put["branch"] != attachBranch || put["content"] != base64.StdEncoding.EncodeToString([]byte("png")) {
		t.Fatalf("put: %v", put)
	}
	if !strings.HasPrefix(link, rp.c.web+"/octo/widgets/blob/"+attachBranch+"/38/") || !strings.HasSuffix(link, "-my-shot-1-.png?raw=true") {
		t.Fatalf("link: %s", link)
	}
}

func TestFetchSignsInOnlyOnItsHost(t *testing.T) {
	_, rp := bound(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token tok" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte("image"))
	}))
	body, ok, err := rp.Fetch(context.Background(), rp.c.web+"/user-attachments/assets/abc")
	if !ok || err != nil {
		t.Fatalf("ok %v, err %v", ok, err)
	}
	data, _ := io.ReadAll(body)
	body.Close()
	if string(data) != "image" {
		t.Fatalf("got %q", data)
	}
	if _, ok, _ := rp.Fetch(context.Background(), "https://example.com/a.png"); ok {
		t.Fatal("claimed a link on another host")
	}
}

func TestFetchReadsRepoFilesRaw(t *testing.T) {
	c := testConn(t, http.NotFoundHandler())
	c.host, c.web = "github.com", "https://github.com"
	var got string
	c.http = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		got = r.URL.String()
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("x")), Request: r}, nil
	})}
	b, _ := c.bind(map[string]string{"repo": "octo/widgets"})
	body, ok, err := b.Fetch(context.Background(), "https://github.com/octo/widgets/blob/"+attachBranch+"/38/a.png?raw=true")
	if !ok || err != nil {
		t.Fatalf("ok %v, err %v", ok, err)
	}
	body.Close()
	if got != "https://raw.githubusercontent.com/octo/widgets/"+attachBranch+"/38/a.png" {
		t.Fatalf("fetched %s", got)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
