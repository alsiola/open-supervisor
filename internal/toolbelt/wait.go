package toolbelt

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/alsiola/open-supervisor/internal/core"
	"github.com/alsiola/open-supervisor/internal/engine"
)

const conditionsHelp = `Conditions (any one of them ends the wait):
  pr:<n>[:merged|closed]     the PR merges (the default) or closes; one that ends the other way ends the wait too
  issue:<key>[:closed]       the issue closes
  issue:<key>:<state>        the agent on the issue ends a turn in building, review, help, done or finished
  job:<name>                 the next run of the project's job ends
Put <project>/ before the number, key or name for another project under the same supervisor: pr:club/88:merged.`

func waitCmd(f *flags) *cobra.Command {
	var until string
	var cancelWait bool
	cmd := &cobra.Command{
		Use:   "wait <condition>... [--until <time>] <file|->",
		Short: "Park the issue until something happens (a PR merges, an issue closes, an agent reports, a job runs) or a time comes",
		Long: "Park the issue until any one of the conditions is met, or the --until time comes, whichever is first. The file says what you're waiting for and why; " +
			"the supervisor resumes your session with what happened. Comments still reach you meanwhile. A new wait replaces the last; --cancel drops it.\n\n" + conditionsHelp,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := open(f, true)
			if err != nil {
				return err
			}
			conds, file := args[:len(args)-1], args[len(args)-1]
			reason, err := readBody(file)
			if err != nil {
				return err
			}
			ctx, cancel := timeout()
			defer cancel()
			var h core.Hold
			if cancelWait {
				if len(conds) > 0 || until != "" {
					return errors.New("--cancel takes no conditions or time")
				}
				h = core.Hold{Cancel: true}
			} else if h, err = s.holdFor(ctx, conds, until); err != nil {
				return err
			}
			return s.postHold(ctx, cmd, h, reason)
		},
	}
	cmd.Flags().StringVar(&until, "until", "", "the latest time to wait until (an ISO instant with Z or an offset, e.g. 2026-10-25T09:00Z); alone, park until then")
	cmd.Flags().BoolVar(&cancelWait, "cancel", false, "drop the wait the issue has")
	return cmd
}

func scheduleCmd(f *flags) *cobra.Command {
	return &cobra.Command{
		Use:   "schedule <time> <file|->",
		Short: "Park the issue until a time (the same as wait --until)",
		Long:  "Park the issue until a time (an ISO instant with Z or an offset, e.g. 2026-10-25T09:00Z). The comment says when you'll pick it up and why; the supervisor resumes your session then.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := open(f, true)
			if err != nil {
				return err
			}
			reason, err := readBody(args[1])
			if err != nil {
				return err
			}
			ctx, cancel := timeout()
			defer cancel()
			h, err := s.holdFor(ctx, nil, args[0])
			if err != nil {
				return err
			}
			return s.postHold(ctx, cmd, h, reason)
		},
	}
}

// postHold posts the reason on the issue with the hold, for the supervisor to read.
func (s *session) postHold(ctx context.Context, cmd *cobra.Command, h core.Hold, reason string) error {
	if s.dryRun {
		fmt.Fprintln(cmd.OutOrStdout(), "dry run: would wait for", h.Describe())
		return nil
	}
	url, err := s.b.Tracker.Comment(ctx, s.key, reason+"\n\n"+s.b.Tracker.HoldMarker(h), s.mark())
	if err != nil {
		return err
	}
	if h.Cancel {
		fmt.Fprintf(cmd.OutOrStdout(), "wait cancelled: %s\n", url)
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "waiting for %s: %s\n", h.Describe(), url)
	return nil
}

// holdFor reads conditions and a time into a hold, checking each names something that exists and hasn't
// happened already.
func (s *session) holdFor(ctx context.Context, conds []string, until string) (core.Hold, error) {
	var h core.Hold
	if until != "" {
		at, err := core.ParseTime(until)
		if err != nil {
			return h, fmt.Errorf("%q isn't a time: use an ISO instant like 2026-10-25T09:00Z", until)
		}
		if !at.After(time.Now()) {
			return h, fmt.Errorf("%s is already past", at.Format(time.RFC3339))
		}
		h.Until = &at
	}
	for _, arg := range conds {
		c, err := core.ParseCond(arg)
		if err != nil {
			return h, err
		}
		if strings.EqualFold(c.Project, s.b.Spec.Name) {
			c.Project = ""
		}
		if err := s.checkCond(ctx, c); err != nil {
			return h, err
		}
		h.Conds = append(h.Conds, c)
	}
	if len(h.Conds) == 0 && h.Until == nil {
		return h, errors.New("say what to wait for: a condition, --until <time>, or both")
	}
	return h, nil
}

// checkCond fails a condition that names nothing, or one already met (so the agent carries on instead).
func (s *session) checkCond(ctx context.Context, c core.Cond) error {
	b := s.b
	if c.Project != "" {
		spec := s.cfg.Project(c.Project)
		if spec == nil {
			return fmt.Errorf("%s: no project here is called %s", c, c.Project)
		}
		other, err := bind(s.cfg, spec)
		if err != nil {
			return err
		}
		b = other
	}
	switch c.Kind {
	case core.CondPR:
		id, _ := strconv.Atoi(c.Ref)
		pr, err := b.Repo.PR(ctx, id)
		if err != nil {
			return fmt.Errorf("%s: couldn't read PR #%d: %w", c, id, err)
		}
		if pr.State != core.PROpen {
			return fmt.Errorf("PR #%d is already %s: no need to wait, carry on now", id, pr.State)
		}
	case core.CondIssue:
		issue, err := b.Tracker.Issue(ctx, c.Ref)
		if err != nil {
			return fmt.Errorf("%s: couldn't read issue %s: %w", c, c.Ref, err)
		}
		if !issue.Open && c.On == "closed" {
			return fmt.Errorf("issue %s is already closed: no need to wait, carry on now", c.Ref)
		}
	case core.CondJob:
		names := engine.JobNames(b.Spec)
		if !slices.Contains(names, c.Ref) {
			if len(names) == 0 {
				return fmt.Errorf("%s: the project has no jobs", c)
			}
			return fmt.Errorf("%s: no job called %s (there are %s)", c, c.Ref, strings.Join(names, ", "))
		}
	}
	return nil
}
