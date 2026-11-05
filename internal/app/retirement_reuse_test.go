package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

type retirementReuseFixture struct {
	retirementFixture
	sources                         []gitcache.RetirementSource
	ensures, inventory, obligations int
	refreshLimit                    int
	stale                           bool
	cancel                          context.CancelFunc
}

func (f *retirementReuseFixture) ListRepositories(context.Context) ([]model.Repository, error) {
	return []model.Repository{{ID: 1, Name: "consumer-a", UpstreamID: 1}, {ID: 2, Name: "consumer-b", UpstreamID: 1}}, nil
}
func (f *retirementReuseFixture) RetirementSources(context.Context, string) ([]gitcache.RetirementSource, error) {
	f.inventory++
	if f.cancel != nil && f.inventory == 2 {
		f.cancel()
	}
	return f.sources, nil
}
func (f *retirementReuseFixture) Ensure(context.Context, string) (gitcache.Repository, error) {
	f.ensures++
	if f.refreshLimit > 0 && f.ensures > f.refreshLimit {
		return gitcache.Repository{}, context.DeadlineExceeded
	}
	return gitcache.Repository{Stale: f.stale}, nil
}
func (f *retirementReuseFixture) LegacyRetirementBlockers(context.Context) ([]string, error) {
	f.obligations++
	if f.obligations%2 == 0 {
		return f.blockers, nil
	}
	return nil, nil
}
func retirementReuse(t *testing.T) (*retirementReuseFixture, *GitFragmentLoader, *fakeFragmentBlobs) {
	t.Helper()
	repo := "forge/team/library"
	sha := strings.Repeat("a", 40)
	blobs := &fakeFragmentBlobs{tags: map[string]string{repo + "@v1": sha}, blobs: map[string]string{sha: testFragmentBody}}
	loader, err := NewGitFragmentLoader(blobs, fakeRegistry{known: map[string]bool{repo: true}, green: map[string]bool{repo + "@" + sha: true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := &retirementReuseFixture{}
	for i := 0; i < 8; i++ {
		f.sources = append(f.sources, gitcache.RetirementSource{Ref: fmt.Sprintf("refs/heads/retained-%d", i), SHA: strings.Repeat("b", 40), Path: ".oberth/build.yaml", Body: []byte("spec: {templates: [{templateRef: {name: '" + repo + "@v1'}}]}")})
	}
	return f, loader, blobs
}
func TestRetirementReusesValidatedFragmentOnlyWithinInvocation(t *testing.T) {
	f, loader, blobs := retirementReuse(t)
	check := LegacyRemovalCheck{Catalog: f, Git: f, Fragments: loader}
	for invocation := 1; invocation <= 2; invocation++ {
		if err := check.Check(t.Context()); err != nil {
			t.Fatal(err)
		}
		if f.ensures != invocation || blobs.tagCalls != invocation || blobs.reachCalls != invocation || len(blobs.readArgs) != invocation {
			t.Fatalf("invocation %d: fresh=%d tag=%d reachable=%d read=%d; repeated refs must reuse only validated proof", invocation, f.ensures, blobs.tagCalls, blobs.reachCalls, len(blobs.readArgs))
		}
		if f.inventory != 2*invocation || f.obligations != 2*invocation {
			t.Fatal("consumer inventory or closing obligations skipped")
		}
	}
}
func TestRetirementRepeatedFragmentDoesNotExhaustRefreshBudget(t *testing.T) {
	f, loader, _ := retirementReuse(t)
	f.refreshLimit = 1
	if err := (LegacyRemovalCheck{Catalog: f, Git: f, Fragments: loader}).Check(t.Context()); err != nil {
		t.Fatalf("one fresh fragment across sixteen retained declarations exhausted refresh budget: %v", err)
	}
}
func TestRetirementReuseRetainsRefusalsAndChecksEveryDeclaration(t *testing.T) {
	for _, mode := range []string{"stale", "missing", "unreachable", "unregistered", "allowlist", "no-green", "nested", "legacy-fragment", "large-fragment", "later-consumer-legacy", "closing-obligation", "cancelled", "distinct-version"} {
		t.Run(mode, func(t *testing.T) {
			f, loader, blobs := retirementReuse(t)
			repo := "forge/team/library"
			sha := strings.Repeat("a", 40)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			want := ""
			switch mode {
			case "stale":
				f.stale = true
				want = "fresh source unavailable"
			case "missing":
				delete(blobs.tags, repo+"@v1")
				want = "tag not found"
			case "unreachable":
				blobs.unreachable = map[string]bool{repo + "@" + sha: true}
				want = "not reachable"
			case "unregistered":
				loader.registry = fakeRegistry{}
				want = "does not host"
			case "allowlist":
				loader.allowlist = map[string]bool{"forge/team/other": true}
				want = "allowlist"
			case "no-green":
				f.sources[0].Body = []byte("spec: {templates: [{templateRef: {name: '" + repo + "@" + sha + "'}}]}")
				loader.registry = fakeRegistry{known: map[string]bool{repo: true}}
				want = "no green run"
			case "nested":
				blobs.blobs[sha] = "spec: {templates: [{templateRef: {name: 'forge/team/other@v1'}}]}"
				want = "nested"
			case "legacy-fragment":
				blobs.blobs[sha] = "metadata: {annotations: {oberth.ci/kvm-templates: main}}"
				want = "legacy declaration"
			case "large-fragment":
				blobs.blobs[sha] = strings.Repeat("x", (1<<20)+1)
				want = "inspection limit"
			case "later-consumer-legacy":
				f.sources[len(f.sources)-1].Body = []byte("metadata: {annotations: {oberth.ci/kvm-templates: main}}")
				want = "legacy declaration"
			case "closing-obligation":
				f.blockers = []string{"unfinished cleanup"}
				want = "unfinished cleanup"
			case "cancelled":
				f.cancel = cancel
				want = "context canceled"
			case "distinct-version":
				f.sources[len(f.sources)-1].Body = []byte("spec: {templates: [{templateRef: {name: '" + repo + "@v2'}}]}")
				want = "tag not found"
			}
			err := (LegacyRemovalCheck{Catalog: f, Git: f, Fragments: loader}).Check(ctx)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("%s refusal lost: %v", mode, err)
			}
			if mode == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation not preserved: %v", err)
			}
		})
	}
}

func TestRetirementMemoBoundFallsBackToCompleteValidation(t *testing.T) {
	f, loader, blobs := retirementReuse(t)
	check := LegacyRemovalCheck{Catalog: f, Git: f, Fragments: loader}
	for _, mode := range []string{"entries", "bytes"} {
		t.Run(mode, func(t *testing.T) {
			memo := retirementFragmentMemo{validated: make(map[argoworkflow.FragmentKey]string)}
			if mode == "entries" {
				for i := 0; i < retirementFragmentMemoEntries; i++ {
					memo.validated[argoworkflow.FragmentKey{Repo: "forge/team/unrelated", Version: fmt.Sprint(i)}] = strings.Repeat("b", 40)
				}
			} else {
				memo.bytes = retirementFragmentMemoBytes
			}
			before, reads := f.ensures, len(blobs.readArgs)
			for i := 0; i < 2; i++ {
				if err := check.checkFragments(t.Context(), []string{"forge/team/library@v1"}, &memo); err != nil {
					t.Fatal(err)
				}
			}
			if f.ensures-before != 2 || len(blobs.readArgs)-reads != 2 {
				t.Fatal("full memo skipped validation or grew beyond budget")
			}
			if _, ok := memo.validated[argoworkflow.FragmentKey{Repo: "forge/team/library", Version: "v1"}]; ok {
				t.Fatal("full memo retained more keys")
			}
		})
	}
}

type retirementFragmentMutation struct {
	FragmentLoader
	change func(*argoworkflow.Fragment)
}

func (m retirementFragmentMutation) Load(ctx context.Context, key argoworkflow.FragmentKey, trigger pipeline.Trigger, kind model.RefKind, run string) (argoworkflow.Fragment, error) {
	fragment, err := m.FragmentLoader.Load(ctx, key, trigger, kind, run)
	if err == nil {
		m.change(&fragment)
	}
	return fragment, err
}
func TestRetirementMemoRequiresCompleteExactProof(t *testing.T) {
	for _, mode := range []string{"key", "invalid-sha", "sha-pin", "nested", "legacy", "load-error"} {
		t.Run(mode, func(t *testing.T) {
			f, loader, blobs := retirementReuse(t)
			ref := "forge/team/library@v1"
			if mode == "sha-pin" {
				ref = "forge/team/library@" + strings.Repeat("a", 40)
			}
			mutation := retirementFragmentMutation{FragmentLoader: loader, change: func(fragment *argoworkflow.Fragment) {
				switch mode {
				case "key":
					fragment.Key.Repo = "forge/team/other"
				case "invalid-sha":
					fragment.SHA = "not-a-commit"
				case "sha-pin":
					fragment.SHA = strings.Repeat("b", 40)
				case "nested":
					fragment.Source = []byte("spec: {templates: [{templateRef: {name: 'forge/team/other@v1'}}]}")
				case "legacy":
					fragment.Source = []byte("metadata: {annotations: {oberth.ci/kvm-templates: main}}")
				}
			}}
			if mode == "load-error" {
				delete(blobs.blobs, strings.Repeat("a", 40))
			}
			memo := retirementFragmentMemo{validated: make(map[argoworkflow.FragmentKey]string)}
			check := LegacyRemovalCheck{Catalog: f, Git: f, Fragments: mutation}
			if err := check.checkFragments(t.Context(), []string{ref}, &memo); err == nil {
				t.Fatal("invalid proof accepted")
			}
			if len(memo.validated) != 0 || memo.bytes != 0 {
				t.Fatal("failed proof cached")
			}
			// A subsequent valid attempt must perform fresh authority and source reads.
			check.Fragments = loader
			blobs.blobs[strings.Repeat("a", 40)] = testFragmentBody
			if err := check.checkFragments(t.Context(), []string{ref}, &memo); err != nil {
				t.Fatal(err)
			}
			if f.ensures != 2 || len(memo.validated) != 1 {
				t.Fatal("failure substituted for fresh proof")
			}
		})
	}
}
func TestRetirementNewInvocationCannotReuseChangedFragmentAuthority(t *testing.T) {
	f, loader, _ := retirementReuse(t)
	check := LegacyRemovalCheck{Catalog: f, Git: f, Fragments: loader}
	if err := check.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	loader.registry = fakeRegistry{}
	if err := check.Check(t.Context()); err == nil || !strings.Contains(err.Error(), "does not host") {
		t.Fatalf("previous invocation proof survived: %v", err)
	}
	if f.ensures != 2 {
		t.Fatalf("fresh lookup count=%d", f.ensures)
	}
}
