package github

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/alsiola/open-supervisor/internal/adapter"
)

// attachBranch holds the files agents attach to comments, apart from the code: an orphan branch, so it shares
// no history with the base branch and runs none of its workflows.
const attachBranch = "open-supervisor-attachments"

// maxAttachment keeps an upload well under what the contents API takes in one request.
const maxAttachment = 50 << 20

var (
	_ adapter.Attacher = (*Tracker)(nil)
	_ adapter.Attacher = (*Repo)(nil)
)

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// Attach commits the file to the attachments branch, under the issue's folder, and links to it. A ?raw=true
// link on github.com serves the file itself, so an image shows inline in a comment, also in a private repo.
// It goes up as the bot when there is one, else (or when the bot may not write contents) as the sign-in.
func (b *binding) Attach(ctx context.Context, key, name string, data []byte) (string, error) {
	if len(data) > maxAttachment {
		return "", fmt.Errorf("%s is %d MB; attachments go up to %d MB", name, len(data)>>20, maxAttachment>>20)
	}
	name = strings.Trim(unsafeName.ReplaceAllString(path.Base(name), "-"), "-")
	if name == "" || name == "." {
		name = "file"
	}
	file := strings.TrimPrefix(key, "#") + "/" + time.Now().UTC().Format("20060102T150405Z") + "-" + name
	err := b.attach(ctx, true, key, file, data)
	if b.c.write != b.c.read && (adapter.IsStatus(err, http.StatusForbidden) || adapter.IsStatus(err, http.StatusNotFound)) {
		err = b.attach(ctx, false, key, file, data)
	}
	if err != nil {
		return "", err
	}
	segs := strings.Split(file, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return fmt.Sprintf("%s/%s/blob/%s/%s?raw=true", b.c.web, b.repo, attachBranch, strings.Join(segs, "/")), nil
}

func (b *binding) attach(ctx context.Context, write bool, key, file string, data []byte) error {
	if err := b.ensureAttachBranch(ctx, write); err != nil {
		return err
	}
	req := map[string]string{
		"message": fmt.Sprintf("Attach %s for %s", path.Base(file), key),
		"content": base64.StdEncoding.EncodeToString(data),
		"branch":  attachBranch,
	}
	_, err := b.c.call(ctx, write, b.repo, http.MethodPut, b.path("/contents/%s", file), req, nil)
	return err
}

// ensureAttachBranch creates the attachments branch the first time: a commit with a README and no parents.
func (b *binding) ensureAttachBranch(ctx context.Context, write bool) error {
	_, err := b.c.call(ctx, write, b.repo, http.MethodGet, b.path("/branches/%s", attachBranch), nil, nil)
	if !adapter.IsStatus(err, http.StatusNotFound) {
		return err
	}
	readme := "Files open-supervisor's agents attached to issue and PR comments, one folder per issue. Nothing here is built or merged.\n"
	var tree, commit struct {
		SHA string `json:"sha"`
	}
	if _, err := b.c.call(ctx, write, b.repo, http.MethodPost, b.path("/git/trees"), map[string]any{
		"tree": []map[string]string{{"path": "README.md", "mode": "100644", "type": "blob", "content": readme}},
	}, &tree); err != nil {
		return err
	}
	if _, err := b.c.call(ctx, write, b.repo, http.MethodPost, b.path("/git/commits"), map[string]any{
		"message": "Start the attachments branch", "tree": tree.SHA, "parents": []string{},
	}, &commit); err != nil {
		return err
	}
	_, err = b.c.call(ctx, write, b.repo, http.MethodPost, b.path("/git/refs"), map[string]string{"ref": "refs/heads/" + attachBranch, "sha": commit.SHA}, nil)
	if adapter.IsStatus(err, http.StatusUnprocessableEntity) {
		return nil // another agent made it first
	}
	return err
}

// Fetch downloads a file from this GitHub host (a comment's attachment, a file in a repo) signed in as the
// sign-in, which a private repository's attachments need. Links elsewhere aren't sent the token.
func (b *binding) Fetch(ctx context.Context, link string) (io.ReadCloser, bool, error) {
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, false, nil
	}
	api, _ := url.Parse(b.c.api)
	switch {
	case u.Host == b.c.host:
		// github.com/<owner>/<repo>/blob/<ref>/<path> is a page; raw.githubusercontent.com serves the file.
		if parts := strings.SplitN(strings.TrimPrefix(u.Path, "/"), "/", 4); b.c.host == "github.com" && len(parts) == 4 && (parts[2] == "blob" || parts[2] == "raw") {
			u = &url.URL{Scheme: "https", Host: "raw.githubusercontent.com", Path: "/" + parts[0] + "/" + parts[1] + "/" + parts[3]}
		}
	case u.Host == api.Host, b.c.host == "github.com" && strings.HasSuffix(u.Host, ".githubusercontent.com"):
	default:
		return nil, false, nil
	}
	tok, err := b.c.read.token(ctx, b.repo)
	if err != nil {
		return nil, true, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, true, err
	}
	// Go drops the header on a redirect to another host, such as the signed storage link an attachment ends at.
	req.Header.Set("Authorization", "token "+tok)
	req.Header.Set("User-Agent", "open-supervisor")
	res, err := b.c.http.Do(req)
	if err != nil {
		return nil, true, err
	}
	if res.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 1024))
		res.Body.Close()
		return nil, true, &adapter.APIError{Method: http.MethodGet, URL: u.String(), Status: res.StatusCode, Body: string(body)}
	}
	return res.Body, true, nil
}
