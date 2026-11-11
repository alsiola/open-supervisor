package config

import (
	"strings"
	"testing"
	"time"
)

func TestJobValidate(t *testing.T) {
	cmd := []string{"true"}
	for i, c := range []struct {
		job  Job
		want string
	}{
		{Job{Name: "j", Command: cmd, Cron: "0 9,13,18 * * *", TimeZone: "Europe/London"}, ""},
		{Job{Name: "j", Command: cmd, Cron: "0 9 * * FRI", Checkout: "fresh", On: &JobTrigger{Events: []string{"push"}, Paths: []string{"data/*.json", "!data/posted.json"}}}, ""},
		{Job{Name: "j", Command: cmd, At: []string{"05:17"}, Status: ".local/x"}, ""},
		{Job{Name: "j", Command: cmd, Cron: "0 9 * *"}, "cron:"},
		{Job{Name: "j", Command: cmd, Cron: "0 9 * * *", TimeZone: "Mars/Olympus"}, "timeZone:"},
		{Job{Name: "j", Command: cmd, At: []string{"09:00"}, TimeZone: "Europe/London"}, "timeZone is for cron"},
		{Job{Name: "j", Command: cmd, At: []string{"09:00"}, Cron: "0 9 * * *"}, "not both"},
		{Job{Name: "j", Command: cmd, At: []string{"9am"}}, "isn't HH:MM"},
		{Job{Name: "j", Command: cmd, EveryMinutes: 5, Cron: "0 9 * * *"}, "everyMinutes or at/cron"},
		{Job{Name: "j", Command: cmd, Cron: "CRON_TZ=Europe/London 0 9 * * *"}, "with timeZone"},
		{Job{Name: "j", Command: cmd, Checkout: "new"}, "fresh or main"},
		{Job{Name: "j", Command: cmd, Cron: "0 9 * * *", Status: "s", Checkout: "fresh"}, "not ones with status"},
		{Job{Name: "j", Command: cmd, EveryMinutes: 5, Status: "s"}, "status is for at or cron"},
		{Job{Name: "j", Command: cmd, On: &JobTrigger{Events: []string{"push"}, Paths: []string{"!"}}}, "empty path"},
		{Job{Name: "j", Command: cmd, At: []string{"05:17"}, Status: "s", Priority: "high", LeadMinutes: 20}, ""},
		{Job{Name: "j", Command: cmd, EveryMinutes: 5, Priority: "high"}, ""},
		{Job{Name: "j", Command: cmd, Priority: "urgent"}, "priority \"urgent\""},
		{Job{Name: "j", Command: cmd, At: []string{"05:17"}, LeadMinutes: 20}, "leadMinutes is for"},
		{Job{Name: "j", Command: cmd, EveryMinutes: 5, Priority: "high", LeadMinutes: 20}, "leadMinutes is for"},
		{Job{Name: "j", Command: cmd, At: []string{"05:17"}, Priority: "high", LeadMinutes: -1}, "negative"},
	} {
		errs := strings.Join(c.job.Validate(), "; ")
		if c.want == "" && errs != "" || c.want != "" && !strings.Contains(errs, c.want) {
			t.Errorf("case %d: %q; want %q", i, errs, c.want)
		}
	}
}

func TestJobNextSlot(t *testing.T) {
	now := time.Date(2026, 11, 11, 17, 0, 0, 0, time.UTC)
	for i, c := range []struct {
		job  Job
		want string
	}{
		{Job{At: []string{"05:17", "17:17"}}, "2026-11-11T17:17:00Z"},
		{Job{At: []string{"05:17", "16:00"}}, "2026-11-12T05:17:00Z"},
		{Job{At: []string{"17:00"}}, "2026-11-12T17:00:00Z"}, // after from, not at it
		{Job{Cron: "30 18 * * *", TimeZone: "Europe/Paris"}, "2026-11-11T17:30:00Z"},
		{Job{EveryMinutes: 5}, "0001-01-01T00:00:00Z"},
	} {
		if got := c.job.NextSlot(now).UTC().Format(time.RFC3339); got != c.want {
			t.Errorf("case %d: %s; want %s", i, got, c.want)
		}
	}
}
