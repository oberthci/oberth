package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type fakeFragmentBlobs struct {
	tags        map[string]string
	blobs       map[string]string
	unreachable map[string]bool
	tagCalls    int
	reachCalls  int
	reachErr    error
	readArgs    []string
}

func (f *fakeFragmentBlobs) TagSHA(_ context.Context, input, tag string) (string, error) {
	f.tagCalls++
	sha, ok := f.tags[input+"@"+tag]
	if !ok {
		return "", errors.New("tag not found")
	}
	return sha, nil
}

func (f *fakeFragmentBlobs) ReachableFromUpstreamDefault(_ context.Context, input, commit string) (bool, error) {
	f.reachCalls++
	if f.reachErr != nil {
		return false, f.reachErr
	}
	return !f.unreachable[input+"@"+commit], nil
}

func (f *fakeFragmentBlobs) ReadBlob(_ context.Context, input, sha, file string, limit int) ([]byte, error) {
	f.readArgs = append(f.readArgs, input+"|"+sha+"|"+file)
	if limit <= 0 {
		return nil, errors.New("bad limit")
	}
	body, ok := f.blobs[sha]
	if !ok {
		return nil, errors.New("blob not found")
	}
	return []byte(body), nil
}

type fakeRegistry struct{ known map[string]bool }

func (f fakeRegistry) RepositoryRegistered(_ context.Context, name string) (bool, error) {
	return f.known[name], nil
}

const testFragmentBody = `apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: run
  templates:
    - name: run
      container:
        image: golang:1.26
`

func testLoader(t *testing.T, allowlist []string) (*GitFragmentLoader, *fakeFragmentBlobs) {
	t.Helper()
	blobs := &fakeFragmentBlobs{
		tags:  map[string]string{"acme/maven-verify@v3": strings.Repeat("a", 40)},
		blobs: map[string]string{strings.Repeat("a", 40): testFragmentBody},
	}
	registry := fakeRegistry{known: map[string]bool{"maven-verify": true}}
	loader, err := NewGitFragmentLoader(blobs, registry, allowlist)
	if err != nil {
		t.Fatal(err)
	}
	return loader, blobs
}

func TestFragmentLoaderReadsThePinnedCommit(t *testing.T) {
	t.Parallel()
	loader, blobs := testLoader(t, nil)
	key := argoworkflow.FragmentKey{Repo: "acme/maven-verify", Version: "v3"}

	fragment, err := loader.Load(context.Background(), key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if fragment.SHA != strings.Repeat("a", 40) {
		t.Fatalf("Load recorded SHA %q, want the commit the tag resolved to", fragment.SHA)
	}
	if string(fragment.Source) != testFragmentBody {
		t.Fatal("Load returned different bytes than the blob holds")
	}
	if len(blobs.readArgs) != 1 || !strings.HasSuffix(blobs.readArgs[0], "|"+argoworkflow.FragmentFile) {
		t.Fatalf("Load read %v, want the fragment file", blobs.readArgs)
	}
}

func TestFragmentLoaderRefusesAnUnregisteredRepository(t *testing.T) {
	t.Parallel()
	loader, blobs := testLoader(t, nil)
	key := argoworkflow.FragmentKey{Repo: "attacker/elsewhere", Version: "v1"}

	if _, err := loader.Load(context.Background(), key); err == nil {
		t.Fatal("a repository this server does not host resolved as a fragment")
	}
	if blobs.tagCalls != 0 {
		t.Fatal("the registration check ran after the git read; it must gate it")
	}
}

func TestFragmentLoaderHonoursTheAllowlist(t *testing.T) {
	t.Parallel()
	key := argoworkflow.FragmentKey{Repo: "acme/maven-verify", Version: "v3"}

	blocked, blobs := testLoader(t, []string{"acme/something-else"})
	if _, err := blocked.Load(context.Background(), key); err == nil {
		t.Fatal("a repository outside the allowlist resolved")
	}
	if blobs.tagCalls != 0 {
		t.Fatal("the allowlist check ran after the git read; it must gate it")
	}

	allowed, _ := testLoader(t, []string{"acme/maven-verify"})
	if _, err := allowed.Load(context.Background(), key); err != nil {
		t.Fatalf("an allowlisted repository was refused: %v", err)
	}
}

func TestFragmentLoaderRefusesAnUnknownTag(t *testing.T) {
	t.Parallel()
	loader, _ := testLoader(t, nil)
	key := argoworkflow.FragmentKey{Repo: "acme/maven-verify", Version: "v99"}

	if _, err := loader.Load(context.Background(), key); err == nil {
		t.Fatal("an unknown tag resolved")
	}
}

func TestFragmentLoaderRefusesAnUnreachableTagCommit(t *testing.T) {
	t.Parallel()
	loader, blobs := testLoader(t, nil)
	blobs.unreachable = map[string]bool{
		"acme/maven-verify@" + strings.Repeat("a", 40): true,
	}
	key := argoworkflow.FragmentKey{Repo: "acme/maven-verify", Version: "v3"}

	_, err := loader.Load(context.Background(), key)
	if err == nil {
		t.Fatal("a tag on a commit unreachable from the upstream default branch resolved as a fragment (#213)")
	}
	if !strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("refusal does not name reachability: %v", err)
	}
	if len(blobs.readArgs) != 0 {
		t.Fatal("the fragment blob was read despite the reachability refusal; the gate must run before the read")
	}
}

func TestFragmentLoaderFailsClosedWhenReachabilityErrors(t *testing.T) {
	t.Parallel()
	loader, blobs := testLoader(t, nil)
	blobs.reachErr = errors.New("cache unavailable")
	key := argoworkflow.FragmentKey{Repo: "acme/maven-verify", Version: "v3"}

	if _, err := loader.Load(context.Background(), key); err == nil {
		t.Fatal("a reachability-check error did not refuse the fragment")
	}
	if len(blobs.readArgs) != 0 {
		t.Fatal("the fragment blob was read despite the reachability error")
	}
}

func TestFragmentLoaderConsultsReachabilityBeforeReading(t *testing.T) {
	t.Parallel()
	loader, blobs := testLoader(t, nil)
	key := argoworkflow.FragmentKey{Repo: "acme/maven-verify", Version: "v3"}

	if _, err := loader.Load(context.Background(), key); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if blobs.reachCalls != 1 {
		t.Fatalf("reachability gate consulted %d times, want exactly once per load (#213)", blobs.reachCalls)
	}
}

func TestLoadFragmentsReadsEveryReferenceOnce(t *testing.T) {
	t.Parallel()
	loader, blobs := testLoader(t, nil)
	source := []byte(`
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  templates:
    - name: main
      steps:
        - - name: one
            templateRef:
              name: acme/maven-verify@v3
              template: run
        - - name: two
            templateRef:
              name: acme/maven-verify@v3
              template: run
`)
	fragments, err := loadFragments(context.Background(), loader, source)
	if err != nil {
		t.Fatalf("loadFragments: %v", err)
	}
	if len(fragments) != 1 {
		t.Fatalf("loaded %d fragments, want the one deduplicated reference", len(fragments))
	}
	if blobs.tagCalls != 1 {
		t.Fatalf("the same fragment was fetched %d times", blobs.tagCalls)
	}
}

func TestLoadFragmentsReturnsNothingForADocumentWithNoReferences(t *testing.T) {
	t.Parallel()
	loader, blobs := testLoader(t, nil)
	source := []byte(`
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  templates:
    - name: main
      container:
        image: golang:1.26
`)
	fragments, err := loadFragments(context.Background(), loader, source)
	if err != nil {
		t.Fatalf("loadFragments: %v", err)
	}
	if len(fragments) != 0 {
		t.Fatalf("loaded %d fragments for a document that references none", len(fragments))
	}
	if blobs.tagCalls != 0 {
		t.Fatal("a document with no references still reached git")
	}
}

func TestLoadFragmentsRefusesReferencesWithNoLoader(t *testing.T) {
	t.Parallel()
	source := []byte(`
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  templates:
    - name: main
      steps:
        - - name: one
            templateRef:
              name: acme/maven-verify@v3
              template: run
`)
	if _, err := loadFragments(context.Background(), nil, source); err == nil {
		t.Fatal("fragment references were silently ignored when no loader is configured")
	}
}
