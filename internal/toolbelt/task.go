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
			"background commands comes, when the supervisor restarts or when the issue finishes. Its output goes to a log; when it ends, the supervisor " +
			"says how on the issue and tells the agent at its next turn. A worktree a task runs from stays until the task ends.",
	}
	var dir string
	start := &cobra.Command{
		Use:   "start <name> [--dir <dir>] -- <command> [args...]",
		Short: "Start a task: runs the command detached from the current directory (or --dir); prints its log",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.ArgsLenAtDash() != 1 {
				return errors.New("name the task, then put the command after --: agent task start backfill -- node backfill.ts")
			}
			s, err := open(f, true)
			if err != nil {
				return err
			}
			name := args[0]
			if dir == "" {
				dir, _ = os.Getwd()
			}
			if dir, err = filepath.Abs(dir); err != nil {
				return err
			}
			file := engine.TaskFile(s.b.StateDir, s.key, name)
			if old, ok := engine.ReadTask(file); ok && old.Running() {
				return fmt.Errorf("the task %s is still running (pid %d, %s so far); stop it with agent task stop %s, or pick another name", name, old.PID, old.Took(), name)
			}
			if s.dryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "dry run: would start %s in %s\n", strings.Join(args[1:], " "), dir)
				return nil
			}
			self, err := os.Executable()
			if err != nil {
				return err
			}
			t := engine.Task{Name: name, Issue: s.key, Command: args[1:], Dir: dir, Log: strings.TrimSuffix(file, ".json") + ".log",
				StartedAt: time.Now().UTC(), Outcome: "running"}
			if err := engine.SaveTask(file, t); err != nil {
				return err
			}
			if t, err = engine.StartTask(self, file, t); err != nil {
				_ = os.Remove(file)
				return fmt.Errorf("couldn't start the task: %w", err)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "started the task %s (pid %d) in %s\n", name, t.PID, dir)
			fmt.Fprintf(out, "the supervisor says on the issue when it ends; its log is\n%s\n", t.Log)
			return nil
		},
	}
	start.Flags().StringVar(&dir, "dir", "", "where to run it (default the current directory)")
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
				if t.EndedAt == nil && !t.Running() {
					state = "lost"
				}
				if t.Exit != nil && state == "failed" {
					state = fmt.Sprintf("failed (exit %d)", *t.Exit)
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
		Short: "Stop a running task and everything it started",
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
