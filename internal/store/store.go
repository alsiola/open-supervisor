// Package store is what the supervisor remembers about a project between runs: one record per issue it has
// claimed, the webhooks it owns, when it last swept and where its timed jobs got to. Kept as JSON in the
// project's state directory and written on every change. Every method is safe to call from any goroutine;
// a record is changed only inside Update, so a change is never lost to another made at the same time.
package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/core"
)

// State is an issue's place in its life under the supervisor.
type State string

const (
	Queued    State = "queued"    // claimed, first turn not started yet
	Running   State = "running"   // a turn is in progress
	Building  State = "building"  // the agent pushed and ended its turn; resumed when the checks land
	Review    State = "review"    // something is up for the owner
	Help      State = "help"      // the agent asked the owner something it can't go on without
	Done      State = "done"      // everything asked is handled; only a merge or close is left
	Finished  State = "finished"  // the PR merged or closed (or the issue closed) and the agent cleaned up
	Scheduled State = "scheduled" // held until RunAt: not started yet, or rescheduled
	Manual    State = "manual"    // the owner is attached to the session; events wait
	Failed    State = "failed"    // the last turn ended in an error
)

// Turn is one turn's record.
type Turn struct {
	Kind      string              `json:"kind"` // start, events
	StartedAt time.Time           `json:"startedAt"`
	EndedAt   *time.Time          `json:"endedAt,omitempty"`
	Cost      float64             `json:"cost,omitempty"`
	Tokens    *core.Tokens        `json:"tokens,omitempty"`
	Models    []string            `json:"models,omitempty"`
	Summary   string              `json:"summary,omitempty"`
	Error     string              `json:"error,omitempty"`
	Events    []core.Event        `json:"events"`
	Handle    *adapter.TurnHandle `json:"handle,omitempty"`
}

// Record is everything the supervisor holds about one claimed issue.
type Record struct {
	Key     string `json:"key"`
	Title   string `json:"title"`
	URL     string `json:"url,omitempty"`
	Session string `json:"session"`
	State   State  `json:"state"`
	// PreviousState is where to go back to when the owner detaches.
	PreviousState State `json:"previousState,omitempty"`
	// PRs belong to this issue: from its branch, and any the agent reports.
	PRs []int `json:"prs"`
	// ClosedPRs were already reported merged or closed, so the agent hears each once.
	ClosedPRs []int `json:"closedPrs"`
	// HeadSHA is the open PR's head; HeadAt is when it was first seen, so a repo with no checks at all isn't
	// waited on for ever; ChecksReported is the head whose checks were last reported.
	HeadSHA        string     `json:"headSha,omitempty"`
	HeadAt         *time.Time `json:"headAt,omitempty"`
	ChecksReported string     `json:"checksReported,omitempty"`
	Preview        string     `json:"preview,omitempty"`
	// Touches is what the agent says the work changes, for the in-flight list other agents read.
	Touches string `json:"touches,omitempty"`
	// Seen are comment ids already handed to the agent (bounded).
	Seen []string `json:"seen"`
	// Since: comments at or before this instant were ingested; the reconcile asks for newer ones.
	Since    time.Time    `json:"since"`
	Queue    []core.Event `json:"queue"`
	Turns    int          `json:"turns"`
	Cost     float64      `json:"cost"`
	Tokens   *core.Tokens `json:"tokens,omitempty"`
	Attempts int          `json:"attempts"`
	// RetryAfter: after a failed turn, no retry before this instant.
	RetryAfter          *time.Time `json:"retryAfter,omitempty"`
	LastTurn            *Turn      `json:"lastTurn,omitempty"`
	IssueClosedReported bool       `json:"issueClosedReported,omitempty"`
	// RunAt, while scheduled: when the next turn is due; ScheduledBy is the comment whose marker set it ("" for
	// the issue body).
	RunAt       *time.Time `json:"runAt,omitempty"`
	ScheduledBy string     `json:"scheduledBy,omitempty"`
	ClaimedAt   time.Time  `json:"claimedAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

// HasPR reports whether the PR belongs to the issue.
func (r *Record) HasPR(id int) bool { return contains(r.PRs, id) }

// AddPR adds a PR to the issue, once.
func (r *Record) AddPR(id int) {
	if id > 0 && !r.HasPR(id) {
		r.PRs = append(r.PRs, id)
	}
}

// HasSeen reports whether the comment was already handed over.
func (r *Record) HasSeen(id string) bool {
	for _, s := range r.Seen {
		if s == id {
			return true
		}
	}
	return false
}

// Clone is a deep enough copy for readers (the TUI) to hold while the record changes.
func (r *Record) Clone() *Record {
	data, _ := json.Marshal(r)
	var c Record
	_ = json.Unmarshal(data, &c)
	return &c
}

func contains(xs []int, x int) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// JobState is where a timed job got to: the last slot started and no retry of a busy one before RetryAfter.
type JobState struct {
	LastSlot   string     `json:"lastSlot,omitempty"`
	RetryAfter *time.Time `json:"retryAfter,omitempty"`
}

// Hook is a webhook the supervisor registered.
type Hook struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// Data is the whole file.
type Data struct {
	Issues     map[string]*Record   `json:"issues"`
	Hooks      map[string]Hook      `json:"hooks,omitempty"`
	LastSweep  string               `json:"lastSweep,omitempty"`
	LastTriage string               `json:"lastTriage,omitempty"`
	Jobs       map[string]*JobState `json:"jobs,omitempty"`
	// Claims are when each issue was last claimed, kept after its record goes, so a labelled issue reopened
	// since can be told from one another supervisor holds.
	Claims map[string]time.Time `json:"claims,omitempty"`
}

const seenKeep = 500

// Store is one project's state file.
type Store struct {
	Dir  string
	file string

	mu       sync.Mutex
	data     Data
	onChange func()
}

// New opens the store in dir; Load reads it.
func New(dir string, onChange func()) *Store {
	if onChange == nil {
		onChange = func() {}
	}
	return &Store{Dir: dir, file: filepath.Join(dir, "state.json"), data: Data{Issues: map[string]*Record{}}, onChange: onChange}
}

// Load reads the state file, if there is one.
func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var d Data
	if err := json.Unmarshal(raw, &d); err != nil {
		return err
	}
	if d.Issues == nil {
		d.Issues = map[string]*Record{}
	}
	s.data = d
	return nil
}

// saveLocked writes the file; the caller holds mu.
func (s *Store) saveLocked() {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return
	}
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return
	}
	tmp := s.file + ".tmp"
	if os.WriteFile(tmp, raw, 0o644) == nil {
		_ = os.Rename(tmp, s.file)
	}
}

func (s *Store) changed() {
	s.saveLocked()
	go s.onChange()
}

// Get returns a copy of the issue's record.
func (s *Store) Get(key string) *Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.data.Issues[key]; r != nil {
		return r.Clone()
	}
	return nil
}

// All returns copies of every record, oldest claim first.
func (s *Store) All() []*Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Record, 0, len(s.data.Issues))
	for _, r := range s.data.Issues {
		out = append(out, r.Clone())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ClaimedAt.Equal(out[j].ClaimedAt) {
			return out[i].Key < out[j].Key
		}
		return out[i].ClaimedAt.Before(out[j].ClaimedAt)
	})
	return out
}

// Active are the records claimed and neither finished nor waiting for their time: what counts against the
// cap and goes in the in-flight list.
func (s *Store) Active() []*Record {
	var out []*Record
	for _, r := range s.All() {
		if r.State != Finished && r.State != Scheduled {
			out = append(out, r)
		}
	}
	return out
}

// ByPR finds the issue a PR belongs to.
func (s *Store) ByPR(id int) *Record {
	for _, r := range s.All() {
		if r.HasPR(id) {
			return r
		}
	}
	return nil
}

// BySHA finds the issue whose open PR has this head. Either side may be abbreviated (Bitbucket's PRs give
// 12-character hashes, its webhooks full ones), so a prefix of at least 7 characters matches.
func (s *Store) BySHA(sha string) *Record {
	for _, r := range s.All() {
		if SameSHA(r.HeadSHA, sha) {
			return r
		}
	}
	return nil
}

// SameSHA reports whether two commit hashes name the same commit, either possibly abbreviated.
func SameSHA(a, b string) bool {
	if len(a) < 7 || len(b) < 7 {
		return a == b && a != ""
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	return strings.EqualFold(a, b[:len(a)])
}

// Create adds a record for a newly claimed issue.
func (s *Store) Create(key, title, url, session string) *Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	r := &Record{Key: key, Title: title, URL: url, Session: session, State: Queued, PRs: []int{}, ClosedPRs: []int{}, Seen: []string{}, Since: now, Queue: []core.Event{}, ClaimedAt: now, UpdatedAt: now}
	s.data.Issues[key] = r
	if s.data.Claims == nil {
		s.data.Claims = map[string]time.Time{}
	}
	s.data.Claims[key] = now
	s.changed()
	return r.Clone()
}

// LastClaim is when the issue was last claimed, zero if never.
func (s *Store) LastClaim(key string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Claims[key]
}

// Update changes a record in place and saves it, returning a copy of the result (nil if there is no record).
func (s *Store) Update(key string, change func(r *Record)) *Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.data.Issues[key]
	if r == nil {
		return nil
	}
	change(r)
	r.UpdatedAt = time.Now().UTC()
	if len(r.Seen) > seenKeep {
		r.Seen = r.Seen[len(r.Seen)-seenKeep:]
	}
	s.changed()
	return r.Clone()
}

// Remove forgets an issue.
func (s *Store) Remove(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Issues, key)
	s.changed()
}

// Prune drops finished records older than a week, so the file doesn't grow for ever.
func (s *Store) Prune() {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	changed := false
	for k, r := range s.data.Issues {
		if r.State == Finished && r.UpdatedAt.Before(cutoff) {
			delete(s.data.Issues, k)
			changed = true
		}
	}
	if changed {
		s.changed()
	}
}

// Hook returns the webhook registered on a target.
func (s *Store) Hook(target string) (Hook, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.data.Hooks[target]
	return h, ok
}

// SetHook records the webhook registered on a target.
func (s *Store) SetHook(target string, h Hook) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Hooks == nil {
		s.data.Hooks = map[string]Hook{}
	}
	s.data.Hooks[target] = h
	s.changed()
}

// LastSweep is the local date of the last sweep.
func (s *Store) LastSweep() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.LastSweep
}

// SetLastSweep records the local date of a sweep.
func (s *Store) SetLastSweep(day string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.LastSweep = day
	s.changed()
}

// LastTriage is the local date of the last triage.
func (s *Store) LastTriage() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.LastTriage
}

// SetLastTriage records the local date of a triage.
func (s *Store) SetLastTriage(day string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.LastTriage = day
	s.changed()
}

// Job changes a timed job's state and saves it.
func (s *Store) Job(name string, change func(j *JobState)) JobState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Jobs == nil {
		s.data.Jobs = map[string]*JobState{}
	}
	j := s.data.Jobs[name]
	if j == nil {
		j = &JobState{}
		s.data.Jobs[name] = j
	}
	if change != nil {
		change(j)
		s.changed()
	}
	return *j
}
