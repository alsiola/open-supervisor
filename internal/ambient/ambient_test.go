package ambient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOpenRouterAsksTheDecisionsEndpoint(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/decisions" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("asked %s with %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"model":"cloudflare/clef-omni-20261009","answers":{"useful":{"type":"noul","noul":0.7358}},"usage":{"input_tokens":187,"output_tokens":0,"cost":0.00002805}}`))
	}))
	defer srv.Close()
	v, err := NewOpenRouter("k", "cloudflare/clef-omni", srv.URL, nil).Judge(context.Background(), Ask{Message: "m", Source: "s", Issue: "i", Progress: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if v.P != 0.7358 || v.Model != "cloudflare/clef-omni-20261009" || v.InputTokens != 187 || v.Cost != 0.00002805 || v.Attempts != 1 {
		t.Fatalf("verdict %+v", v)
	}
	state, _ := got["state"].(map[string]any)
	q, _ := got["questions"].(map[string]any)["useful"].(map[string]any)
	if got["model"] != "cloudflare/clef-omni" || state["message"] != "m" || state["issue"] != "i" || state["progress"] != "p" || q["type"] != "noul" || q["instructions"] != Question {
		t.Fatalf("request %v", got)
	}
}

func TestOpenRouterTriesAgainOnlyWhenItMayHelp(t *testing.T) {
	var calls atomic.Int32
	status := http.StatusBadGateway
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"busy"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"model":"m","answers":{"useful":{"noul":0.2}},"usage":{}}`))
	}))
	defer srv.Close()
	j := NewOpenRouter("k", "m", srv.URL, nil)
	j.Backoff = time.Millisecond
	v, err := j.Judge(context.Background(), Ask{})
	if err != nil || v.P != 0.2 || v.Attempts != 3 {
		t.Fatalf("after two 502s: %+v %v", v, err)
	}
	calls.Store(0)
	status = http.StatusBadRequest
	if _, err := j.Judge(context.Background(), Ask{}); err == nil || calls.Load() != 1 {
		t.Fatalf("a 400 was tried %d times (%v)", calls.Load(), err)
	}
}

func TestOpenRouterWithoutAProbabilityFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"model":"m","answers":{}}`))
	}))
	defer srv.Close()
	if _, err := NewOpenRouter("k", "m", srv.URL, nil).Judge(context.Background(), Ask{}); err == nil {
		t.Fatal("an answer without a probability passed")
	}
}

func TestShareBodyQuotesAndLinks(t *testing.T) {
	src := "The tunnel drops webhook deliveries while it reconnects.\n\nI replay the missed ones from GitHub's delivery log on reconnect."
	b := Body(Origin{Issue: "#12", Title: "Tunnel restarts", Where: "on PR #34", URL: "https://github.com/o/r/pull/34#issuecomment-1"}, src)
	for _, want := range []string{Header, "from #12 (Tunnel restarts)", "on PR #34", "not checked here", "> The tunnel drops webhook deliveries", ">\n> I replay", "Original: https://github.com/o/r/pull/34#issuecomment-1"} {
		if !strings.Contains(b, want) {
			t.Errorf("share lacks %q:\n%s", want, b)
		}
	}
	if !IsShare(b) {
		t.Fatal("a share isn't recognised as one")
	}
}

func TestIsShareSurvivesFormatting(t *testing.T) {
	for _, body := range []string{
		"**" + Header + "** from #1. …",
		"*" + Header + "* from ENG-1",
		"h3. " + Header + " from ENG-1", // not a leading run of punctuation: shouldn't count
		"Some text\n\n" + Header + " from #2",
	} {
		want := !strings.HasPrefix(body, "h3.")
		if IsShare(body) != want {
			t.Errorf("IsShare(%q) = %v", body, !want)
		}
	}
	if IsShare("I think we should have it say " + Header + " in the middle") {
		t.Error("a mention mid-line counted as a share")
	}
}

func TestSubstantiveAndExcerpt(t *testing.T) {
	if Substantive("Working on this on branch `agent/issue-72`. A PR will follow here.") {
		t.Error("a start comment counted")
	}
	if !Substantive(strings.Repeat("finding ", 20)) {
		t.Error("a long comment didn't count")
	}
	long := strings.Repeat("a", 2000) + "\n\n" + strings.Repeat("b", 2000)
	if e := Excerpt(long, 3000); !strings.HasSuffix(e, "[…]") || strings.Contains(e, "b") {
		t.Errorf("excerpt not cut at the paragraph: %d chars", len(e))
	}
	if Excerpt("short", 3000) != "short" {
		t.Error("a short comment was cut")
	}
}

func TestLedgerRemembersAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ambient", "ledger.json")
	l := OpenLedger(path)
	if l.Seen("demo/c1") {
		t.Fatal("seen before anything happened")
	}
	if !l.Begin("demo/c1") || l.Begin("demo/c1") {
		t.Fatal("a second pass on a busy source was let start")
	}
	l.End("demo/c1")
	l.MarkSeen("demo/c1")
	l.Decide("demo/c1", "demo/2", 0.9, "https://x/2#1")
	l.Decide("demo/c1", "demo/3", 0.1, "")
	l.Fail("demo/c1", "demo/4", 0, errors.New("502"))
	l = OpenLedger(path)
	if !l.Seen("demo/c1") {
		t.Fatal("forgot the source")
	}
	if e, _ := l.Get("demo/c1", "demo/2"); !e.Done() || e.Shared != "https://x/2#1" {
		t.Fatalf("entry %+v", e)
	}
	if got := l.Retry("demo/c1"); len(got) != 1 || got[0] != "demo/4" {
		t.Fatalf("retry %v", got)
	}
	l.Fail("demo/c1", "demo/4", 0, errors.New("502"))
	l.Fail("demo/c1", "demo/4", 0, errors.New("502"))
	if got := l.Retry("demo/c1"); len(got) != 0 {
		t.Fatalf("still retrying after %d failures: %v", MaxAttempts, got)
	}
}

func TestLedgerForgetsOldEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	old := map[string]Entry{"demo/c0": {Decided: true, At: time.Now().Add(-40 * 24 * time.Hour)}}
	data, _ := json.Marshal(old)
	_ = os.WriteFile(path, data, 0o644)
	if OpenLedger(path).Seen("demo/c0") {
		t.Fatal("a 40-day-old entry was kept")
	}
}

// TestClefLive puts representative pairs to the real model. It runs only with OPENROUTER_API_KEY set. Findings
// that bear on an issue, one from another part of the system, should clear the default threshold (0.4), and a
// status update or same-area chatter shouldn't.
func TestClefLive(t *testing.T) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		t.Skip("OPENROUTER_API_KEY not set")
	}
	const threshold = 0.4
	j := NewOpenRouter(key, "cloudflare/clef-omni", "", nil)
	for _, c := range []struct {
		name, message, issue string
		useful               bool
	}{
		{"same area, a constraint the issue needs",
			"Heads up: GitHub's check-suite webhook doesn't fire for checks from third-party apps like Netlify; I had to poll the commit status API instead.",
			"Wait for the checks the base branch requires before reporting a pass\n\nThe deploy preview check sometimes reports after we've told the agent checks passed.", true},
		{"another part of the system, a shared cause",
			"The worktree setup failed with ENOSPC: the disk had 2GB left because old worktrees from finished issues are never removed.",
			"Turns fail at random with 'error writing transcript'\n\nSome agent turns die mid-way with an I/O error from the transcript writer.", true},
		{"status update",
			"Pushed the fix for review in PR #64; checks are running and I'll report once they pass and the preview is up.",
			"Agents sometimes sit in building for hours after their checks finish\n\nNobody resumes them until the next reconcile.", false},
		{"same area, nothing the issue needs",
			"Renamed the jobs tab in the TUI and tidied its column widths; no behaviour changes.",
			"The TUI's issue list flickers when a turn starts\n\nThe list redraws twice for every event.", false},
	} {
		v, err := j.Judge(context.Background(), Ask{Message: c.message, Source: "Posted on the issue by the agent working on #64 in project supervisor", Issue: c.issue, Progress: "State: building"})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: p=%.3f cost=$%.6f", c.name, v.P, v.Cost)
		if (v.P >= threshold) != c.useful {
			t.Errorf("%s: p=%.3f, want useful=%v", c.name, v.P, c.useful)
		}
	}
}
