// Package all registers every adapter that ships with open-supervisor. To add one, write its package under
// internal/adapters, register its Spec in init, and import it here.
package all

import (
	_ "github.com/alsiola/open-supervisor/internal/adapters/bitbucket"
	_ "github.com/alsiola/open-supervisor/internal/adapters/claudecode"
	_ "github.com/alsiola/open-supervisor/internal/adapters/github"
	_ "github.com/alsiola/open-supervisor/internal/adapters/jira"
	_ "github.com/alsiola/open-supervisor/internal/adapters/posthog"
	_ "github.com/alsiola/open-supervisor/internal/adapters/tunnel"
)
