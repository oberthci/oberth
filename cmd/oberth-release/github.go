package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/oberthci/oberth/internal/releaseverify"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

// The release store is the GitHub release of each tag in the repository that
// the canonical origin names, written through the GitHub REST API.
const (
	githubAPI     = "https://api.github.com"
	githubUploads = "https://uploads.github.com"
)

type publication struct {
	api, uploads, repository, token, dir string
	client                               *http.Client
}

type githubRelease struct {
	ID     int64         `json:"id"`
	Tag    string        `json:"tag_name"`
	Draft  bool          `json:"draft"`
	Assets []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// apiCall is one GitHub request; a body is JSON unless contentType says otherwise.
type apiCall struct {
	method, address, contentType string
	body                         io.Reader
	size                         int64
}

func publicationFor(o options) (publication, error) {
	token, err := os.ReadFile(o.token)
	if err != nil || len(bytes.TrimSpace(token)) == 0 {
		return publication{}, releaseToken
	}
	if err := privateTokenFile(o.token, o.dir); err != nil {
		return publication{}, err
	}
	if o.origin != releaseverify.Origin {
		return publication{}, originDiffers
	}
	repository, ok := githubRepository(releaseverify.Origin)
	if !ok {
		return publication{}, inputs
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return publication{api: githubAPI, uploads: githubUploads, repository: repository, token: strings.TrimSpace(string(token)), dir: o.dir, client: client}, nil
}

// privateTokenFile accepts the token file only when group and others have no
// access to it and it lies outside the release directory, so neither another
// user nor a publication of that directory can expose it.
func privateTokenFile(token, dir string) error {
	info, err := os.Stat(token)
	if err != nil || !info.Mode().IsRegular() {
		return releaseToken
	}
	if info.Mode().Perm()&0o077 != 0 {
		return tokenShared
	}
	file, err := realPath(token)
	if err != nil {
		return releaseToken
	}
	root, err := realPath(dir)
	if err != nil {
		return nil
	}
	if relative, err := filepath.Rel(root, file); err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return tokenInside
	}
	return nil
}

func realPath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return filepath.Abs(resolved)
}

// githubRepository reads owner/name from https://github.com/<owner>/<name>/releases/download.
func githubRepository(origin string) (string, bool) {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", false
	}
	parts := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
	if len(parts) != 4 || parts[0] == "" || parts[1] == "" || parts[2] != "releases" || parts[3] != "download" {
		return "", false
	}
	return parts[0] + "/" + parts[1], true
}

func (p publication) repo(format string, values ...any) string {
	return p.api + "/repos/" + p.repository + fmt.Sprintf(format, values...)
}

func get(address string) apiCall { return apiCall{method: http.MethodGet, address: address} }

func jsonCall(method, address string, value any) (apiCall, error) {
	body, err := json.Marshal(value)
	return apiCall{method: method, address: address, body: bytes.NewReader(body), size: int64(len(body))}, err
}

// send performs one call and decodes a successful JSON answer into into.
func (p publication) send(ctx context.Context, call apiCall, into any) (int, error) {
	request, err := http.NewRequestWithContext(ctx, call.method, call.address, call.body)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Authorization", "Bearer "+p.token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if call.body != nil {
		request.ContentLength = call.size
		request.Header.Set("Content-Type", cmp.Or(call.contentType, "application/json"))
	}
	response, err := p.client.Do(request)
	if err != nil {
		return 0, err
	}
	defer func() { _ = response.Body.Close() }()
	if into == nil || response.StatusCode/100 != 2 {
		return response.StatusCode, nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, releaseverify.MaxMetadata+1))
	if err != nil {
		return 0, err
	}
	if len(body) > releaseverify.MaxMetadata || json.Unmarshal(body, into) != nil {
		return 0, fmt.Errorf("%w: %s %s answered with invalid JSON", storeFailure, call.method, call.address)
	}
	return response.StatusCode, nil
}

func refused(status int, call apiCall) error {
	return fmt.Errorf("%w: GitHub answered %d to %s %s", storeFailure, status, call.method, call.address)
}

// expect sends call and refuses any status other than want.
func (p publication) expect(ctx context.Context, call apiCall, want int, into any) error {
	status, err := p.send(ctx, call, into)
	if err == nil && status != want {
		err = refused(status, call)
	}
	return err
}

// tagPublished refuses before any write when the tag is not on GitHub, so
// publishing a draft never makes GitHub create the tag itself.
func (p publication) tagPublished(ctx context.Context, tag string) error {
	call := get(p.repo("/git/ref/tags/%s", url.PathEscape(tag)))
	status, err := p.send(ctx, call, nil)
	switch {
	case err != nil:
		return err
	case status == http.StatusNotFound:
		return tagMissing
	case status != http.StatusOK:
		return refused(status, call)
	}
	return nil
}

// releaseFor returns the tag's release, a draft left by an earlier attempt, or
// a new draft. A draft is not reachable by its tag, so it is looked for among
// the newest releases.
func (p publication) releaseFor(ctx context.Context, tag string) (githubRelease, error) {
	var release githubRelease
	call := get(p.repo("/releases/tags/%s", url.PathEscape(tag)))
	status, err := p.send(ctx, call, &release)
	if err != nil || status == http.StatusOK {
		return release, err
	}
	if status != http.StatusNotFound {
		return release, refused(status, call)
	}
	var newest []githubRelease
	if err := p.expect(ctx, get(p.repo("/releases?per_page=10")), http.StatusOK, &newest); err != nil {
		return release, err
	}
	for _, candidate := range newest {
		if candidate.Draft && candidate.Tag == tag {
			return candidate, nil
		}
	}
	create, err := jsonCall(http.MethodPost, p.repo("/releases"), map[string]any{"tag_name": tag, "name": tag, "draft": true})
	if err != nil {
		return release, err
	}
	return release, p.expect(ctx, create, http.StatusCreated, &release)
}

func (p publication) upload(ctx context.Context, id int64, name string) error {
	file, err := os.Open(filepath.Join(p.dir, name))
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	address := fmt.Sprintf("%s/repos/%s/releases/%d/assets?name=%s", p.uploads, p.repository, id, url.QueryEscape(name))
	call := apiCall{method: http.MethodPost, address: address, contentType: "application/octet-stream", body: file, size: info.Size()}
	return p.expect(ctx, call, http.StatusCreated, nil)
}

// publishDraft makes the release public without making it latest; only
// finalization after clean verification moves latest.
func (p publication) publishDraft(ctx context.Context, id int64) error {
	call, err := jsonCall(http.MethodPatch, p.repo("/releases/%d", id), map[string]any{"draft": false, "make_latest": "false"})
	if err != nil {
		return err
	}
	return p.expect(ctx, call, http.StatusOK, nil)
}

// latestTag is GitHub's latest release: the newest non-draft, non-prerelease
// release it marks latest. No release yet is the empty tag.
func (p publication) latestTag(ctx context.Context) (string, error) {
	var release githubRelease
	call := get(p.repo("/releases/latest"))
	status, err := p.send(ctx, call, &release)
	switch {
	case err != nil:
		return "", err
	case status == http.StatusNotFound:
		return "", nil
	case status != http.StatusOK:
		return "", refused(status, call)
	case !workflowpolicy.IsReleaseVersion(release.Tag):
		return "", fmt.Errorf("%w: latest is %q", latestInvalid, release.Tag)
	}
	return release.Tag, nil
}

func (p publication) makeLatest(ctx context.Context, tag string) error {
	var release githubRelease
	if err := p.expect(ctx, get(p.repo("/releases/tags/%s", url.PathEscape(tag))), http.StatusOK, &release); err != nil {
		return err
	}
	call, err := jsonCall(http.MethodPatch, p.repo("/releases/%d", release.ID), map[string]any{"make_latest": "true"})
	if err != nil {
		return err
	}
	return p.expect(ctx, call, http.StatusOK, nil)
}
