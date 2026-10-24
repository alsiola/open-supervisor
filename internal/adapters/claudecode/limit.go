package claudecode

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// limitHit reports whether a turn stopped on the subscription's usage limit, and when the limit lifts if claude
// said. The API error on the result or the assistant's stand-in message says so first-hand; claude's wording
// ("You've hit your session limit · resets 9:40am (Europe/London)") covers versions that don't carry it.
func limitHit(r *result, stdout string, now time.Time) (bool, *time.Time) {
	text, _ := r.Result.(string)
	hit := r.APIError == "usage_limit_reached" || limitText.MatchString(text)
	var resets *time.Time
	for _, line := range strings.Split(stdout, "\n") {
		if !strings.Contains(line, `"resetsAt"`) || !strings.Contains(line, `"rejected"`) {
			continue
		}
		var m struct {
			APIError  string    `json:"api_error"`
			RateLimit rateLimit `json:"rate_limit_info"`
			Params    struct {
				RateLimit rateLimit `json:"rate_limit_info"`
			} `json:"api_error_params"`
		}
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		if m.APIError == "usage_limit_reached" {
			hit = true
		}
		// The assistant's error and the rate_limit_event before it both carry the window that turned it away.
		for _, rl := range []rateLimit{m.Params.RateLimit, m.RateLimit} {
			if rl.Status == "rejected" && rl.ResetsAt > 0 {
				t := time.Unix(rl.ResetsAt, 0).UTC()
				resets = &t
			}
		}
	}
	if !hit {
		return false, nil
	}
	if resets == nil {
		resets = resetFromText(text, now)
	}
	return true, resets
}

type rateLimit struct {
	Status   string `json:"status"`
	ResetsAt int64  `json:"resetsAt"`
}

var (
	limitText = regexp.MustCompile(`(?i)you've hit your .*limit`)
	// "resets 9:40am (Europe/London)", "resets 3am (Europe/London)", "resets Oct 27, 3pm (Europe/London)".
	resetText = regexp.MustCompile(`(?i)resets (?:([A-Z][a-z]{2}) (\d{1,2}),? )?(\d{1,2})(?::(\d{2}))?\s*(am|pm)(?: \(([^)]+)\))?`)
)

// resetFromText reads the reset time out of claude's limit message: the next such time after now, in the zone
// it names (the local one if it names none or one this machine doesn't know).
func resetFromText(text string, now time.Time) *time.Time {
	m := resetText.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	loc := time.Local
	if m[6] != "" {
		if l, err := time.LoadLocation(m[6]); err == nil {
			loc = l
		}
	}
	hour, _ := strconv.Atoi(m[3])
	minute, _ := strconv.Atoi(m[4])
	if hour == 12 {
		hour = 0
	}
	if strings.EqualFold(m[5], "pm") {
		hour += 12
	}
	local := now.In(loc)
	if m[1] != "" {
		month, err := time.Parse("Jan", m[1])
		if err != nil {
			return nil
		}
		day, _ := strconv.Atoi(m[2])
		t := time.Date(local.Year(), month.Month(), day, hour, minute, 0, 0, loc)
		if t.Before(now.Add(-24 * time.Hour)) {
			t = t.AddDate(1, 0, 0)
		}
		t = t.UTC()
		return &t
	}
	t := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	t = t.UTC()
	return &t
}
