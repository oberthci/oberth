package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// githubFake keeps releases in memory and answers the REST calls publication
// makes, refusing what GitHub refuses: a second asset of one name, deletes.
type githubFake struct {
	mu        sync.Mutex
	t         *testing.T
	tags      map[string]string
	annotated map[string]bool
	releases  []*fakeRelease
	latest    string
	corrupt   string
	calls     []string
	bodies    []map[string]any
}

type fakeRelease struct {
	githubRelease
	bytes map[string][]byte
}

const fakeRepository = "/repos/oberthci/oberth"

// fakeCommit is the commit every fixture tag and signed record names.
var fakeCommit = strings.Repeat("a", 40)

func githubFixture(t *testing.T) (publication, *githubFake) {
	t.Helper()
	fake := &githubFake{t: t, tags: map[string]string{"v1.0.0": fakeCommit, "v2.0.0": fakeCommit, "v3.0.0": fakeCommit}, annotated: map[string]bool{}}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "artifact"), []byte("release bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	return publication{api: server.URL, uploads: server.URL, repository: "oberthci/oberth", token: "fixture", dir: dir, client: server.Client()}, fake
}

func (f *githubFake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.RequestURI())
	if r.Header.Get("Authorization") != "Bearer fixture" || !strings.HasPrefix(r.URL.Path, fakeRepository+"/") {
		f.t.Error("missing authorization or other repository", r.URL)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, fakeRepository)
	switch {
	case r.Method == http.MethodGet:
		f.read(w, path)
	case r.Method == http.MethodPost && path == "/releases":
		f.create(w, r)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/assets"):
		f.upload(w, r, path)
	case r.Method == http.MethodPatch && strings.HasPrefix(path, "/releases/"):
		f.update(w, r, path)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *githubFake) read(w http.ResponseWriter, path string) {
	var release *fakeRelease
	switch {
	case strings.HasPrefix(path, "/git/ref/tags/"), strings.HasPrefix(path, "/git/tags/"):
		f.tagObject(w, path)
		return
	case path == "/releases":
		f.answer(w, true, f.list())
		return
	case strings.HasPrefix(path, "/releases/tags/"):
		release = f.find(strings.TrimPrefix(path, "/releases/tags/"), false)
	case path == "/releases/latest":
		release = f.find(f.latest, false)
	case strings.HasPrefix(path, "/releases/"):
		release = f.byID(path)
	}
	f.answer(w, release != nil, release)
}

// tagObject answers a tag reference with its commit, or for an annotated tag
// with a tag object that /git/tags/ resolves to the commit.
func (f *githubFake) tagObject(w http.ResponseWriter, path string) {
	object := gitObject{}
	if tag, ok := strings.CutPrefix(path, "/git/ref/tags/"); ok {
		object.Object.Type, object.Object.SHA = "commit", f.tags[tag]
		if f.annotated[tag] {
			object.Object.Type, object.Object.SHA = "tag", "tag-object-"+tag
		}
	} else {
		object.Object.Type, object.Object.SHA = "commit", f.tags[strings.TrimPrefix(path, "/git/tags/tag-object-")]
	}
	f.answer(w, object.Object.SHA != "", object)
}

func (f *githubFake) answer(w http.ResponseWriter, ok bool, value any) {
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err := json.NewEncoder(w).Encode(value); err != nil {
		f.t.Error(err)
	}
}

func (f *githubFake) find(tag string, drafts bool) *fakeRelease {
	for _, release := range f.releases {
		if release.Tag == tag && (drafts || !release.Draft) {
			return release
		}
	}
	return nil
}

func (f *githubFake) byID(path string) *fakeRelease {
	id, _ := strconv.ParseInt(strings.Split(strings.TrimPrefix(path, "/releases/"), "/")[0], 10, 64)
	for _, release := range f.releases {
		if release.ID == id {
			return release
		}
	}
	return nil
}

func (f *githubFake) list() []githubRelease {
	releases := []githubRelease{}
	for index := len(f.releases) - 1; index >= 0; index-- {
		releases = append(releases, f.releases[index].githubRelease)
	}
	return releases
}

func (f *githubFake) body(r *http.Request) map[string]any {
	var value map[string]any
	if err := json.NewDecoder(r.Body).Decode(&value); err != nil {
		f.t.Error(err)
	}
	f.bodies = append(f.bodies, value)
	return value
}

func (f *githubFake) create(w http.ResponseWriter, r *http.Request) {
	value := f.body(r)
	tag, _ := value["tag_name"].(string)
	if f.find(tag, false) != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}
	draft, _ := value["draft"].(bool)
	release := f.add(tag, draft)
	w.WriteHeader(http.StatusCreated)
	f.answer(w, true, release.githubRelease)
}

func (f *githubFake) add(tag string, draft bool) *fakeRelease {
	release := &fakeRelease{githubRelease: githubRelease{ID: int64(len(f.releases) + 1), Tag: tag, Draft: draft}, bytes: map[string][]byte{}}
	f.releases = append(f.releases, release)
	return release
}

func (f *githubFake) upload(w http.ResponseWriter, r *http.Request, path string) {
	release := f.byID(path)
	name := r.URL.Query().Get("name")
	if release == nil || r.Header.Get("Content-Type") != "application/octet-stream" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if _, ok := release.bytes[name]; ok {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Error(err)
		return
	}
	if name == f.corrupt {
		body = append(body, " changed in transit"...)
	}
	release.put(name, body)
	w.WriteHeader(http.StatusCreated)
}

func (r *fakeRelease) put(name string, body []byte) {
	r.bytes[name] = body
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(body))
	r.Assets = append(r.Assets, githubAsset{Name: name, State: "uploaded", Digest: digest, Size: int64(len(body))})
}

func (f *githubFake) update(w http.ResponseWriter, r *http.Request, path string) {
	release := f.byID(path)
	if release == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	value := f.body(r)
	if draft, ok := value["draft"].(bool); ok {
		release.Draft = draft
	}
	if value["make_latest"] == "true" && !release.Draft {
		f.latest = release.Tag
	}
	f.answer(w, true, release.githubRelease)
}

// writes lists the calls that change GitHub.
func (f *githubFake) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var writes []string
	for _, call := range f.calls {
		if !strings.HasPrefix(call, http.MethodGet) {
			writes = append(writes, call)
		}
	}
	return writes
}
