package core

import (
	"testing"
	"time"
)

func TestParseCond(t *testing.T) {
	cases := map[string]string{
		"pr:1257":                "pr:1257:merged",
		"pr:#12:closed":          "pr:12:closed",
		"pr:club/88:merged":      "pr:club/88:merged",
		"issue:PROJ-12":          "issue:PROJ-12:closed",
		"issue:12:review":        "issue:12:review",
		"Issue:club/#4:finished": "issue:club/4:finished",
		"job:backup":             "job:backup",
		"job:club/nightly-sync":  "job:club/nightly-sync",
	}
	for in, want := range cases {
		c, err := ParseCond(in)
		if err != nil || c.String() != want {
			t.Errorf("%q: got %q %v, want %q", in, c, err, want)
		}
		if back, err := ParseCond(c.String()); err != nil || back != c {
			t.Errorf("%q doesn't read back: %+v %v", c, back, err)
		}
	}
	for _, bad := range []string{"", "pr", "pr:abc", "pr:12:open", "issue:12:started", "job:backup:ok", "branch:main", "pr:/12", "pr:1:2:3"} {
		if _, err := ParseCond(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestHoldIn(t *testing.T) {
	h, ok := HoldIn("Waiting on the schema PR.\n\n<!-- waitFor: pr:1257:merged issue:club/12:review; until 2026-10-25T09:00Z -->")
	if !ok || len(h.Conds) != 2 || h.Until == nil || h.Until.Format(time.RFC3339) != "2026-10-25T09:00:00Z" {
		t.Fatalf("waitFor: %+v %v", h, ok)
	}
	if h.Conds[1] != (Cond{Kind: CondIssue, Project: "club", Ref: "12", On: "review"}) {
		t.Errorf("second condition: %+v", h.Conds[1])
	}
	if got, _ := HoldIn("[waitFor: job:backup]"); len(got.Conds) != 1 || got.Until != nil {
		t.Errorf("bracketed: %+v", got)
	}
	// The last marker wins, whichever kind.
	if got, _ := HoldIn("<!-- waitFor: pr:1 --> then <!-- runAt: 2026-10-25T09:00Z -->"); len(got.Conds) != 0 || got.Until == nil {
		t.Errorf("runAt after waitFor: %+v", got)
	}
	if got, _ := HoldIn("<!-- runAt: 2026-10-25T09:00Z --> then <!-- waitFor: cancel -->"); !got.Cancel {
		t.Errorf("cancel: %+v", got)
	}
	// A bad marker is skipped, not taken as an empty wait.
	if got, ok := HoldIn("<!-- waitFor: pr:1 --> <!-- waitFor: nonsense -->"); !ok || len(got.Conds) != 1 {
		t.Errorf("bad marker: %+v %v", got, ok)
	}
	for _, h := range []Hold{{Conds: h.Conds, Until: h.Until}, {Until: h.Until}, {Conds: h.Conds[:1]}, {Cancel: true}} {
		back, ok := HoldIn("<!-- " + h.Spec() + " -->")
		if !ok || back.Spec() != h.Spec() {
			t.Errorf("%q doesn't read back: %+v", h.Spec(), back)
		}
	}
}

func TestMeets(t *testing.T) {
	merged := Cond{Kind: CondPR, Ref: "12", On: "merged"}
	if !merged.Meets(Fact{Kind: CondPR, Project: "web", Ref: "12", On: "merged"}, "web") {
		t.Error("the merge")
	}
	if !merged.Meets(Fact{Kind: CondPR, Project: "web", Ref: "12", On: "closed"}, "web") {
		t.Error("a PR closed unmerged ends a wait for its merge")
	}
	if merged.Meets(Fact{Kind: CondPR, Project: "api", Ref: "12", On: "merged"}, "web") {
		t.Error("another project's PR #12")
	}
	other := Cond{Kind: CondIssue, Project: "club", Ref: "PROJ-3", On: "review"}
	if !other.Meets(Fact{Kind: CondIssue, Project: "club", Ref: "proj-3", On: "review"}, "web") {
		t.Error("another project's issue reaching review")
	}
	if other.Meets(Fact{Kind: CondIssue, Project: "club", Ref: "PROJ-3", On: "building"}, "web") {
		t.Error("the wrong state")
	}
}
