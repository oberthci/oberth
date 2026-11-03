package gitcache

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func TestGreenIssueTrailersRealGit(t *testing.T) {
	r := newTestRepository(t)
	if err := os.MkdirAll(filepath.Join(r.work, ".oberth"), 0700); err != nil {
		t.Fatal(err)
	}
	base := r.commitFile(t, "policy", ".oberth/issues.yaml", "close_from_trailers: true\n")
	r.commit(t, "prose\n\nThis fixes #99 in the body.\nFixes: #98\n\nThis is ordinary prose after the apparent trailer.", "body\n")
	one := r.commit(t, "repair\n\nFixes: #1, #2\nRefs: #3 #4\nSigned-off-by: Test <test@example.invalid>", "one\n")
	two := r.commit(t, "repair again\n\nCloses: #5\nFixes: #6\nRefs: #6\nFixes: #7\n #8\nRefs: #0, #9999999999999999999999999999\nFixes: prose #9", "two\n")
	c := newTestCache(t, r.upstream)
	if _, err := c.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	enabled, got, err := c.GreenIssueTrailers(t.Context(), "example", base, two)
	if err != nil {
		t.Fatal(err)
	}
	want := []model.CommitIssueTrailer{{IssueID: 1, SHA: one, Close: true}, {IssueID: 2, SHA: one, Close: true}, {IssueID: 3, SHA: one}, {IssueID: 4, SHA: one}, {IssueID: 5, SHA: two, Close: true}, {IssueID: 6, SHA: two, Close: true}, {IssueID: 7, SHA: two, Close: true}, {IssueID: 8, SHA: two, Close: true}}
	if !enabled || !reflect.DeepEqual(got, want) {
		t.Fatalf("trailers enabled=%v got=%#v want=%#v", enabled, got, want)
	}
	enabled, got, err = c.GreenIssueTrailers(t.Context(), "example", two, two)
	if err != nil || !enabled || len(got) != 0 {
		t.Fatalf("same head = %v %#v %v", enabled, got, err)
	}
}

func TestGreenIssueTrailersPolicyAndBounds(t *testing.T) {
	r := newTestRepository(t)
	if err := os.MkdirAll(filepath.Join(r.work, ".oberth"), 0700); err != nil {
		t.Fatal(err)
	}
	heads := []struct {
		body          string
		fail, enabled bool
	}{
		{body: "close_from_trailers: false\n"},
		{body: "unknown: true\n", fail: true},
		{body: "close_from_trailers: [\n", fail: true},
		{body: "close_from_trailers: true\n---\nclose_from_trailers: false\n", fail: true},
		{body: strings.Repeat("#", 16<<10) + "\n", fail: true},
		{body: "close_from_trailers: true\n", enabled: true},
	}
	c := newTestCache(t, r.upstream)
	if _, err := c.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	enabled, got, err := c.GreenIssueTrailers(t.Context(), "example", "", r.initialSHA)
	if err != nil || enabled || len(got) != 0 {
		t.Fatalf("missing policy = %v %#v %v", enabled, got, err)
	}
	for _, tt := range heads {
		sha := r.commitFile(t, "policy\n\nFixes: #1", ".oberth/issues.yaml", tt.body)
		if _, err := c.Ensure(t.Context(), "example"); err != nil {
			t.Fatal(err)
		}
		enabled, _, err := c.GreenIssueTrailers(t.Context(), "example", r.initialSHA, sha)
		if (err != nil) != tt.fail || enabled != tt.enabled || errors.Is(err, model.ErrIssueTrailerPermanent) != tt.fail {
			t.Fatalf("policy %q = %v %v", tt.body, enabled, err)
		}
	}
	if _, _, err := c.GreenIssueTrailers(t.Context(), "example", "", "--all"); err == nil {
		t.Fatal("accepted an option as a head")
	}
}

func TestIssueTrailerFramingRejectsPartialHistory(t *testing.T) {
	for _, raw := range []string{"invalid\x00Fixes: #1\x00", strings.Repeat("a", 40) + "\x00Fixes: #1", strings.Repeat("a", 40) + "\x00Fixes: #1\x00injected", strings.Repeat("a", 40) + "\x00Refs: #1\x00Closes: #2\x00"} {
		if got, err := parseIssueTrailers([]byte(raw)); !errors.Is(err, model.ErrIssueTrailerPermanent) || len(got) != 0 {
			t.Fatalf("accepted corrupt framing %q", raw)
		}
	}
}

func TestGreenIssueTrailersHistoryBoundIsPermanent(t *testing.T) {
	r := newTestRepository(t)
	if err := os.MkdirAll(filepath.Join(r.work, ".oberth"), 0700); err != nil {
		t.Fatal(err)
	}
	base := r.commitFile(t, "policy", ".oberth/issues.yaml", "close_from_trailers: true\n")
	message := filepath.Join(t.TempDir(), "message")
	if err := os.WriteFile(message, []byte("large history\n\nRefs: "+strings.Repeat("#1 ", (4<<20)/3+1)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, r.work, "commit", "--allow-empty", "-F", message)
	runGit(t, r.work, "push", "origin", "HEAD:main")
	head := runGit(t, r.work, "rev-parse", "HEAD")
	c := newTestCache(t, r.upstream)
	if _, err := c.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	enabled, changes, err := c.GreenIssueTrailers(t.Context(), "example", base, head)
	if !errors.Is(err, model.ErrIssueTrailerPermanent) || enabled || len(changes) != 0 || !strings.Contains(err.Error(), "4194304") {
		t.Fatalf("history overflow: enabled=%v changes=%d err=%v", enabled, len(changes), err)
	}
}

func TestGreenIssueTrailersPolicyModeIsPermanent(t *testing.T) {
	for _, mode := range []string{"symlink", "directory"} {
		t.Run(mode, func(t *testing.T) {
			r := newTestRepository(t)
			policy := filepath.Join(r.work, ".oberth", "issues.yaml")
			if err := os.MkdirAll(filepath.Dir(policy), 0700); err != nil {
				t.Fatal(err)
			}
			if mode == "symlink" {
				if err := os.Symlink("other.yaml", policy); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(policy, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(policy, "child"), []byte("invalid"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			runGit(t, r.work, "add", ".oberth")
			runGit(t, r.work, "commit", "-m", "policy mode")
			runGit(t, r.work, "push", "origin", "HEAD:main")
			head := runGit(t, r.work, "rev-parse", "HEAD")
			c := newTestCache(t, r.upstream)
			if _, err := c.Ensure(t.Context(), "example"); err != nil {
				t.Fatal(err)
			}
			if _, changes, err := c.GreenIssueTrailers(t.Context(), "example", "", head); !errors.Is(err, model.ErrIssueTrailerPermanent) || len(changes) != 0 {
				t.Fatalf("policy mode changes=%d err=%v", len(changes), err)
			}
		})
	}
}

func TestGreenIssueTrailersMissingCacheIsRetryable(t *testing.T) {
	r := newTestRepository(t)
	c := newTestCache(t, r.upstream)
	if _, changes, err := c.GreenIssueTrailers(t.Context(), "example", "", r.initialSHA); err == nil || errors.Is(err, model.ErrIssueTrailerPermanent) || len(changes) != 0 {
		t.Fatalf("missing cache: changes=%d err=%v", len(changes), err)
	}
}
