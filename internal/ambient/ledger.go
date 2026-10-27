package ambient

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// MaxAttempts is how many times a pair that failed (the model or the tracker) is tried, each time its source
// comment is seen again, before it is given up on.
const MaxAttempts = 3

// Entry is what is known about one source comment and one target issue.
type Entry struct {
	// Decided: the model answered and, if it said yes, the share was posted. Nothing more happens to the pair.
	Decided bool    `json:"decided,omitempty"`
	P       float64 `json:"p,omitempty"`
	// Shared is the share's link, when it was posted.
	Shared   string    `json:"shared,omitempty"`
	Failures int       `json:"failures,omitempty"`
	Error    string    `json:"error,omitempty"`
	At       time.Time `json:"at"`
}

// Done reports whether nothing more should happen to the pair: decided, or failed too often.
func (e Entry) Done() bool { return e.Decided || e.Failures >= MaxAttempts }

// Ledger remembers every pair asked about, so a comment seen twice (a webhook delivered again, the reconcile
// reading it again) is neither asked about nor shared twice. Kept as JSON, written on every change; entries
// older than a month go when it is read.
type Ledger struct {
	path string
	mu   sync.Mutex
	m    map[string]Entry
	// busy are the sources being worked on now, so two sightings at once don't both start.
	busy map[string]bool
}

// OpenLedger reads the ledger at path, or starts an empty one.
func OpenLedger(path string) *Ledger {
	l := &Ledger{path: path, m: map[string]Entry{}, busy: map[string]bool{}}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &l.m)
	}
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	for k, e := range l.m {
		if e.At.Before(cutoff) {
			delete(l.m, k)
		}
	}
	return l
}

// PairKey names a source comment and a target issue.
func PairKey(source, target string) string { return source + " -> " + target }

// Get is what is known about a pair.
func (l *Ledger) Get(source, target string) (Entry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.m[PairKey(source, target)]
	return e, ok
}

// Seen reports whether a source has had its first pass, against the issues active when it was first seen. Later
// sightings only try again the pairs that failed, so an old comment isn't put to issues claimed since.
func (l *Ledger) Seen(source string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.m[source]
	return ok
}

// Retry are the targets a source still has to try again: failed, and not too often.
func (l *Ledger) Retry(source string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	prefix := PairKey(source, "")
	for k, e := range l.m {
		if strings.HasPrefix(k, prefix) && !e.Done() {
			out = append(out, k[len(prefix):])
		}
	}
	sort.Strings(out)
	return out
}

// MarkSeen records a source's first pass.
func (l *Ledger) MarkSeen(source string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.m[source] = Entry{Decided: true, At: time.Now().UTC()}
	l.saveLocked()
}

// Begin claims a source for one pass; false when a pass on it is already going.
func (l *Ledger) Begin(source string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.busy[source] {
		return false
	}
	l.busy[source] = true
	return true
}

// End lets the source be worked on again.
func (l *Ledger) End(source string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.busy, source)
}

// Decide records the model's answer for a pair, and the share's link when one was posted.
func (l *Ledger) Decide(source, target string, p float64, shared string) {
	l.update(source, target, func(e *Entry) {
		e.Decided, e.P, e.Shared, e.Error = true, p, shared, ""
	})
}

// Fail records a failed attempt at a pair; p is the answer when the model gave one before posting failed.
func (l *Ledger) Fail(source, target string, p float64, err error) {
	l.update(source, target, func(e *Entry) {
		e.Failures++
		e.P = p
		e.Error = err.Error()
	})
}

func (l *Ledger) update(source, target string, change func(e *Entry)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := PairKey(source, target)
	e := l.m[k]
	change(&e)
	e.At = time.Now().UTC()
	l.m[k] = e
	l.saveLocked()
}

func (l *Ledger) saveLocked() {
	if data, err := json.MarshalIndent(l.m, "", "  "); err == nil {
		_ = os.MkdirAll(filepath.Dir(l.path), 0o755)
		tmp := l.path + ".tmp"
		if os.WriteFile(tmp, data, 0o644) == nil {
			_ = os.Rename(tmp, l.path)
		}
	}
}
