package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// tokenSource hands out a bearer token for calls on a repo ("" for calls on no repo).
type tokenSource interface {
	token(ctx context.Context, repo string) (string, error)
	invalidate(repo string)
}

// staticToken is a personal access token.
type staticToken string

func (t staticToken) token(context.Context, string) (string, error) { return string(t), nil }
func (t staticToken) invalidate(string)                             {}

// ghToken is the gh CLI's login, read once and again after a 401.
type ghToken struct {
	host string
	bin  string
	mu   sync.Mutex
	tok  string
}

func (g *ghToken) token(ctx context.Context, _ string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.tok != "" {
		return g.tok, nil
	}
	args := []string{"auth", "token"}
	if g.host != "github.com" {
		args = append(args, "--hostname", g.host)
	}
	if g.bin == "" {
		g.bin = "gh"
	}
	cmd := exec.CommandContext(ctx, g.bin, args...)
	// A token in the environment would be printed instead of the login.
	cmd.Env = withoutGHToken(os.Environ())
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return "", fmt.Errorf("gh auth token: %s (run `gh auth login`)", strings.TrimSpace(string(exit.Stderr)))
		}
		return "", fmt.Errorf("gh auth token: %w (is the gh CLI installed?)", err)
	}
	g.tok = strings.TrimSpace(string(out))
	if g.tok == "" {
		return "", errors.New("gh auth token printed nothing (run `gh auth login`)")
	}
	return g.tok, nil
}

func (g *ghToken) invalidate(string) {
	g.mu.Lock()
	g.tok = ""
	g.mu.Unlock()
}

// appToken is a GitHub App: a JWT for the app itself, and installation tokens for each repo it is installed on,
// cached until shortly before they expire.
type appToken struct {
	id  string
	key *rsa.PrivateKey
	c   *conn

	mu    sync.Mutex
	inst  map[string]int64 // repo → installation
	cache map[int64]cachedToken
}

type cachedToken struct {
	tok     string
	expires time.Time
}

func newAppToken(id, pemText string, c *conn) (*appToken, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("app needs an App ID")
	}
	key, err := parseKey(pemText)
	if err != nil {
		return nil, err
	}
	return &appToken{id: strings.TrimSpace(id), key: key, c: c, inst: map[string]int64{}, cache: map[int64]cachedToken{}}, nil
}

func parseKey(text string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(text)))
	if block == nil {
		return nil, errors.New("the app's private key isn't PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("the app's private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("the app's private key isn't RSA")
	}
	return rk, nil
}

// jwt signs a short-lived token that authenticates as the app itself (RS256).
func (a *appToken) jwt() (string, error) {
	enc := base64.RawURLEncoding
	now := time.Now()
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"iat": now.Add(-60 * time.Second).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": a.id})
	unsigned := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + enc.EncodeToString(sig), nil
}

func (a *appToken) token(ctx context.Context, repo string) (string, error) {
	if repo == "" {
		return a.jwt()
	}
	jwt, err := a.jwt()
	if err != nil {
		return "", err
	}
	client := a.c.client("Bearer " + jwt)
	a.mu.Lock()
	id, ok := a.inst[repo]
	a.mu.Unlock()
	if !ok {
		var inst struct {
			ID int64 `json:"id"`
		}
		if _, err := client.Do(ctx, http.MethodGet, "/repos/"+repo+"/installation", nil, &inst); err != nil {
			return "", fmt.Errorf("app %s isn't installed on %s: %w", a.id, repo, err)
		}
		id = inst.ID
		a.mu.Lock()
		a.inst[repo] = id
		a.mu.Unlock()
	}
	a.mu.Lock()
	cached, ok := a.cache[id]
	a.mu.Unlock()
	if ok && time.Until(cached.expires) > 5*time.Minute {
		return cached.tok, nil
	}
	var t struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if _, err := client.Do(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", id), nil, &t); err != nil {
		return "", fmt.Errorf("installation token for %s: %w", repo, err)
	}
	a.mu.Lock()
	a.cache[id] = cachedToken{t.Token, t.ExpiresAt}
	a.mu.Unlock()
	return t.Token, nil
}

func (a *appToken) invalidate(repo string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if id, ok := a.inst[repo]; ok {
		delete(a.cache, id)
	}
	delete(a.inst, repo)
}

func withoutGHToken(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, "GH_TOKEN=") && !strings.HasPrefix(kv, "GITHUB_TOKEN=") {
			out = append(out, kv)
		}
	}
	return out
}
