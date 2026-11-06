package toolbelt

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/alsiola/open-supervisor/internal/adapter"
)

// gitCredentialCmd is the git credential helper the supervisor sets for an agent's pushes to an SSH remote, which
// it sends over HTTPS: git asks it for a username and password, and it answers with the project's own code-host
// connection. Agents never run it themselves.
func gitCredentialCmd(f *flags) *cobra.Command {
	return &cobra.Command{
		Use:    "git-credential <get|store|erase>",
		Short:  "Sign git in over HTTPS as the project's code-host connection (a git credential helper)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Read what git asks first, so it never writes into a closed pipe.
			_, _ = io.Copy(io.Discard, cmd.InOrStdin())
			// git also tells a helper what worked (store) and what didn't (erase); there is nothing to keep.
			if args[0] != "get" {
				return nil
			}
			s, err := open(f, false)
			if err != nil {
				return err
			}
			signer, ok := s.b.Repo.(adapter.GitSigner)
			if !ok {
				return errors.New(s.b.Repo.Describe() + " can't sign git in")
			}
			ctx, cancel := timeout()
			defer cancel()
			user, pass, err := signer.GitCredential(ctx)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "username=%s\npassword=%s\n", user, pass)
			return nil
		},
	}
}
