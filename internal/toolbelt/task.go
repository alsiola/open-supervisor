package toolbelt

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/alsiola/open-supervisor/internal/engine"
)

func taskCmd(f *flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "task",
		Short: "Run a long command that outlives the turn and the issue (a backfill, a cache fill), and see or stop it",
		Long: "A task is a command the supervisor runs detached for the issue: it isn't stopped when the turn ends, when the agent's own limit on " +
			"background commands comes, when the agent's session ends or restarts, when the supervisor restarts or when the issue finishes, and it has no time " +
			"limit. Its output goes to a log; when it ends, the supervisor says how on the issue (the exit code, or the signal that killed " +
			"it) and tells the agent at its next turn. A task killed by SIGKILL with nothing in its log was most often ended by the system " +
			"for memory: run fewer or smaller tasks at once. A worktree a task runs from stays until the task ends.",
	}
	var dir string
	var now bool
	start := &cobra.Command{
		Use: "start <name> [--dir <dir>] [--now] -- <command> [args...]",
		Short: "Start a task: runs the command detached from the current directory (or --dir); prints its log. " +
			"--now starts it even while the host is busy, once a person has said to go on (alone with the name, it starts a waiting or held task)",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			again := len(args) == 1 && cmd.ArgsLenAtDash() == -1
			if again && !now {
				return errors.New("put the command after --: agent task start backfill -- node backfill.ts (or add --now to start a waiting or held task by its name)")
			}
			if !again && cmd.ArgsLenAtDash() != 1 {
				return errors.New("name the task, then put the command after --: agent task start backfill -- node backfill.ts")
			}
			s, err := open(f, true)
			if err != nil {
				return err
			}
			name := args[0]
			file := engine.TaskFile(s.b.StateDir, s.key, name)
			command := args[1:]
			if again {
				old, ok := engine.Waiting(file)
				if !ok {
					return fmt.Errorf("%s has no task called %s waiting for the host or held; give its command after --", s.key, name)
				}
				command = old.Command
				if dir == "" {
					dir = old.Dir
				}
			}
			if dir == "" {
				dir, _ = os.Getwd()
			}
			if dir, err = filepath.Abs(dir); err != nil {
				return err
			}
			if old, ok := engine.ReadTask(file); ok && old.Running() {
				return fmt.Errorf("the task %s is still running (pid %d, %s so far); stop it with agent task stop %s, or pick another name", name, old.PID, old.Took(), name)
			} else if ok && old.Queued() && !now {
				return fmt.Errorf("the task %s is already waiting for the host to have room (%s so far); stop it with agent task stop %s, or pick another name", name, old.Took(), name)
			}
			if s.dryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "dry run: would start %s in %s\n", strings.Join(command, " "), dir)
				return nil
			}
			self, err := os.Executable()
			if err != nil {
				return err
			}
			at := time.Now().UTC()
			t := engine.Task{Name: name, Issue: s.key, Command: command, Dir: dir, Log: strings.TrimSuffix(file, ".json") + ".log",
				StartedAt: at, Outcome: "running"}
			out := cmd.OutOrStdout()
			busy := ""
			if !now {
				busy = engine.HostBusy(s.cfg.Settings.Host)
			}
			if busy != "" {
				t.Outcome, t.QueuedAt, t.CheckedAt, t.Busy = "queued", &at, &at, busy
				if err := engine.SaveTask(file, t); err != nil {
					return err
				}
				fmt.Fprintf(out, "registered the task %s, but the host is too busy to start it now (%s)\n", name, busy)
				fmt.Fprintf(out, "the supervisor looks again every %s and starts it once there is room, then says on the issue when it ends, as for any task\n", span(engine.TaskRetryEvery))
				fmt.Fprintf(out, "if a person says to go on anyway, agent task start --now %s starts it at once\n", name)
				fmt.Fprintf(out, "if it still can't start after %s, the supervisor says so on the issue and wakes you. End the turn as you would with the task running; its log will be\n%s\n", span(engine.TaskHoldFor), t.Log)
				return nil
			}
			// Saved as running first, so the supervisor no longer takes a task that was waiting as queued.
			if err := engine.SaveTask(file, t); err != nil {
				return err
			}
			if t, err = engine.StartTask(self, file, t); err != nil {
				_ = os.Remove(file)
				return fmt.Errorf("couldn't start the task: %w", err)
			}
			fmt.Fprintf(out, "started the task %s (pid %d) in %s\n", name, t.PID, dir)
			fmt.Fprintf(out, "the supervisor says on the issue when it ends; its log is\n%s\n", t.Log)
			return nil
		},
	}
	start.Flags().StringVar(&dir, "dir", "", "where to run it (default the current directory)")
	start.Flags().BoolVar(&now, "now", false, "start it even while the host is busy: only once a person has said to go on")
	var all bool
	list := &cobra.Command{
		Use:   "list",
		Short: "The issue's tasks (--all: the project's), running or ended, with their logs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := open(f, !all)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			n := 0
			for _, t := range engine.Tasks(s.b.StateDir) {
				if !all && t.Issue != s.key {
					continue
				}
				state := t.Outcome
				if t.EndedAt == nil && !t.Live() {
					state = "lost"
				}
				if state == "failed" {
					state = engine.TaskFailure(t)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", t.Issue, t.Name, state, t.Took(), strings.Join(t.Command, " "), t.Log)
				n++
			}
			_ = w.Flush()
			if n == 0 {
				fmt.Fprintln(out, "no tasks")
			}
			fmt.Fprintf(out, "records: %s\n", engine.TasksDir(s.b.StateDir))
			return nil
		},
	}
	list.Flags().BoolVar(&all, "all", false, "every issue's tasks in the project")
	stop := &cobra.Command{
		Use:   "stop <name>",
		Short: "Stop a running task and everything it started, or drop one waiting for the host or held",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := open(f, true)
			if err != nil {
				return err
			}
			file := engine.TaskFile(s.b.StateDir, s.key, args[0])
			if _, ok := engine.ReadTask(file); !ok {
				return fmt.Errorf("%s has no task called %s", s.key, args[0])
			}
			t, err := engine.StopTask(file)
			if err != nil {
				return err
			}
			if t.Held() {
				fmt.Fprintf(cmd.OutOrStdout(), "dropped the held task %s, which never started\n", t.Name)
				return nil
			}
			if t.PID == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "dropped the task %s before it started\n", t.Name)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "stopping the task %s (pid %d)\n", t.Name, t.PID)
			return nil
		},
	}
	exec := &cobra.Command{
		Use:    "exec <record>",
		Short:  "Run a task's command and record how it ended (what task start runs detached)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return engine.ExecTask(args[0])
		},
	}
	cmd.AddCommand(start, list, stop, exec)
	return cmd
}

// span says a whole number of hours or minutes in words: "2 hours", "15 minutes".
func span(d time.Duration) string {
	n, unit := int(d/time.Minute), "minute"
	if d >= time.Hour && d%time.Hour == 0 {
		n, unit = int(d/time.Hour), "hour"
	}
	if n != 1 {
		unit += "s"
	}
	return fmt.Sprintf("%d %s", n, unit)
}
