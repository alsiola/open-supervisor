package config

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/zalando/go-keyring"
	"gopkg.in/yaml.v3"
)

// Secrets never go in the config file itself. A secret field holds a reference instead:
//
//	keyring:<name>   the OS keyring (macOS Keychain, Secret Service, Windows Credential Manager)
//	file:<name>      ~/.config/open-supervisor/secrets.yaml (0600), for machines with no keyring
//	env:<VAR>        an environment variable
//	cmd:<command>    the output of a shell command (`op read …`, `pass show …`)
//
// Anything else is taken as the secret itself, which works but leaves it in the config.

var (
	secretsMu    sync.Mutex
	resolveCache = map[string]string{}
)

// ResolveSecret turns a reference into the secret.
func ResolveSecret(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", nil
	}
	kind, rest, ok := strings.Cut(ref, ":")
	if !ok {
		return ref, nil
	}
	switch kind {
	case "env":
		v := os.Getenv(rest)
		if v == "" {
			return "", fmt.Errorf("$%s is not set", rest)
		}
		return v, nil
	case "keyring":
		secretsMu.Lock()
		defer secretsMu.Unlock()
		if v, ok := resolveCache[ref]; ok {
			return v, nil
		}
		v, err := keyring.Get(Name, rest)
		if err != nil {
			return "", fmt.Errorf("keyring %s: %w", rest, err)
		}
		resolveCache[ref] = v
		return v, nil
	case "file":
		all, err := readSecretsFile()
		if err != nil {
			return "", err
		}
		v, ok := all[rest]
		if !ok {
			return "", fmt.Errorf("no %s in %s", rest, secretsFile())
		}
		return v, nil
	case "cmd":
		secretsMu.Lock()
		defer secretsMu.Unlock()
		if v, ok := resolveCache[ref]; ok {
			return v, nil
		}
		out, err := exec.Command("sh", "-c", rest).Output()
		if err != nil {
			return "", fmt.Errorf("secret command %q: %w", rest, err)
		}
		v := strings.TrimSpace(string(out))
		resolveCache[ref] = v
		return v, nil
	}
	// "https://…" and the like aren't references.
	return ref, nil
}

// IsReference reports whether a value is a secret reference rather than a secret.
func IsReference(v string) bool {
	kind, _, ok := strings.Cut(v, ":")
	return ok && (kind == "env" || kind == "keyring" || kind == "file" || kind == "cmd")
}

// StoreSecret keeps a secret under a name and returns the reference to put in the config: the keyring when the
// machine has one, else the secrets file.
func StoreSecret(name, value string) (string, error) {
	if err := keyring.Set(Name, name, value); err == nil {
		secretsMu.Lock()
		resolveCache["keyring:"+name] = value
		secretsMu.Unlock()
		return "keyring:" + name, nil
	}
	all, err := readSecretsFile()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if all == nil {
		all = map[string]string{}
	}
	all[name] = value
	data, err := yaml.Marshal(all)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(secretsFile()), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(secretsFile(), data, 0o600); err != nil {
		return "", err
	}
	return "file:" + name, nil
}

func secretsFile() string { return filepath.Join(ConfigDir(), "secrets.yaml") }

func readSecretsFile() (map[string]string, error) {
	data, err := os.ReadFile(secretsFile())
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	return out, yaml.Unmarshal(data, &out)
}
