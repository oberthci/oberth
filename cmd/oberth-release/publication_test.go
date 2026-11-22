package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/releaseverify"
)

// Covers: REQ-C13-013
func TestMonotonicReleaseFinalizationContract(t *testing.T) {
	t.Run("immutable version objects", immutableObjects)
	for _, tc := range []latestCase{
		{"first release", "", "v1.0.0", "v1.0.0"},
		{"newer", "v1.0.0", "v2.0.0", "v2.0.0"},
		{"older", "v3.0.0", "v2.0.0", "v3.0.0"},
		{"equal", "v2.0.0", "v2.0.0", "v2.0.0"},
	} {
		t.Run(tc.name, func(t *testing.T) { checkLatest(t, tc) })
	}
}

type latestCase struct{ name, current, want, expected string }

func checkLatest(t *testing.T, tc latestCase) {
	t.Helper()
	p, fake := githubFixture(t)
	for _, tag := range []string{tc.current, tc.want} {
		if tag != "" && fake.find(tag, false) == nil {
			fake.add(tag, false)
		}
	}
	fake.latest = tc.current
	if err := p.finish(t.Context(), tc.want, io.Discard); err != nil {
		t.Fatal(err)
	}
	if fake.latest != tc.expected {
		t.Fatal("latest differs", fake.latest)
	}
	if moved := len(fake.writes()) > 0; moved != (tc.expected != tc.current) {
		t.Fatal("latest was rewritten without moving forward", fake.writes())
	}
}

func immutableObjects(t *testing.T) {
	t.Helper()
	p, fake := githubFixture(t)
	if err := p.publishFiles(t.Context(), "v2.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := p.publishFiles(t.Context(), "v2.0.0"); err != nil {
		t.Fatal("equal rerun", err)
	}
	if err := os.WriteFile(filepath.Join(p.dir, "artifact"), []byte("changed bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := p.publishFiles(t.Context(), "v2.0.0"); !errors.Is(err, immutable) {
		t.Fatal("changed version bytes accepted", err)
	}
	release := fake.find("v2.0.0", false)
	if release == nil || string(release.bytes["artifact"]) != "release bytes" || len(fake.releases) != 1 {
		t.Fatal("version bytes were changed or a second release was made")
	}
}

func TestPublicationPublishesADraftWithoutMakingItLatest(t *testing.T) {
	p, fake := githubFixture(t)
	if err := p.publishFiles(t.Context(), "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	want := []string{"POST " + fakeRepository + "/releases", "POST " + fakeRepository + "/releases/1/assets?name=artifact", "PATCH " + fakeRepository + "/releases/1"}
	if !reflect.DeepEqual(fake.writes(), want) {
		t.Fatal("publication writes differ", fake.writes())
	}
	if fake.bodies[0]["draft"] != true || fake.bodies[1]["draft"] != false || fake.bodies[1]["make_latest"] != "false" || fake.latest != "" {
		t.Fatal("release was public before its files or became latest", fake.bodies)
	}
}

func TestPublicationResumesAnEarlierDraft(t *testing.T) {
	p, fake := githubFixture(t)
	if err := os.WriteFile(filepath.Join(p.dir, "release.json"), []byte("record"), 0644); err != nil {
		t.Fatal(err)
	}
	draft := fake.add("v1.0.0", true)
	draft.put("artifact", []byte("release bytes"))
	if err := p.publishFiles(t.Context(), "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	want := []string{"POST " + fakeRepository + "/releases/1/assets?name=release.json", "PATCH " + fakeRepository + "/releases/1"}
	if !reflect.DeepEqual(fake.writes(), want) || draft.Draft || len(fake.releases) != 1 {
		t.Fatal("draft was not resumed", fake.writes())
	}
}

func TestPublicationChecksExistingAssetsBeforeAnyWrite(t *testing.T) {
	for _, tc := range []struct {
		name  string
		asset githubAsset
		want  error
	}{
		{"different bytes", githubAsset{Name: "artifact", State: "uploaded", Digest: "sha256:" + strings.Repeat("0", 64), Size: 13}, immutable},
		{"different size", githubAsset{Name: "artifact", State: "uploaded", Digest: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("release bytes"))), Size: 1}, immutable},
		{"incomplete upload", githubAsset{Name: "artifact", State: "starter"}, assetUnconfirmed},
		{"no digest", githubAsset{Name: "artifact", State: "uploaded", Size: 13}, assetUnconfirmed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, fake := githubFixture(t)
			if err := os.WriteFile(filepath.Join(p.dir, "SHA256SUMS"), []byte("sums"), 0644); err != nil {
				t.Fatal(err)
			}
			release := fake.add("v1.0.0", false)
			release.Assets = append(release.Assets, tc.asset)
			if err := p.publishFiles(t.Context(), "v1.0.0"); !errors.Is(err, tc.want) {
				t.Fatal("existing asset was not refused", err)
			}
			if len(fake.writes()) != 0 {
				t.Fatal("a refused publication wrote", fake.writes())
			}
		})
	}
}

func TestPublicationRefusesAMissingTagBeforeAnyWrite(t *testing.T) {
	p, fake := githubFixture(t)
	if err := p.publishFiles(t.Context(), "v9.0.0"); !errors.Is(err, tagMissing) {
		t.Fatal("missing tag not refused", err)
	}
	if len(fake.writes()) != 0 {
		t.Fatal("missing tag reached a write", fake.writes())
	}
}

func TestPublicationCommitsManifestLast(t *testing.T) {
	p, fake := githubFixture(t)
	names := append(publishedNames(), "SHA256SUMS", "SHA256SUMS.sigstore.json")
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(p.dir, name), []byte("bytes "+name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.publishFiles(t.Context(), "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	writes := fake.writes()
	var last []string
	for _, write := range writes[len(writes)-4 : len(writes)-1] {
		last = append(last, write[strings.LastIndex(write, "=")+1:])
	}
	if !reflect.DeepEqual(last, []string{"release.json", "SHA256SUMS", "SHA256SUMS.sigstore.json"}) {
		t.Fatal("manifest published before artifacts", writes)
	}
}

func TestPublicationDoesNotUploadImageInputs(t *testing.T) {
	p, fake := githubFixture(t)
	for _, name := range []string{"oberth.oci", "oberth-source-plugin.oci"} {
		if err := os.Mkdir(filepath.Join(p.dir, name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.publishFiles(t.Context(), "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	release := fake.find("v1.0.0", false)
	if release == nil || len(release.bytes) != 1 || string(release.bytes["artifact"]) != "release bytes" {
		t.Fatal("push inputs were published")
	}
}

func TestReleaseStoreRefusals(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer server.Close()
	p := publication{api: server.URL, uploads: server.URL, repository: "oberthci/oberth", client: server.Client(), dir: t.TempDir()}
	if err := os.WriteFile(filepath.Join(p.dir, "artifact"), []byte("bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := p.publishFiles(t.Context(), "v1.0.0"); !errors.Is(err, storeFailure) || !strings.Contains(err.Error(), "500") {
		t.Fatal("failed write accepted or status hidden", err)
	}
	if err := p.finish(t.Context(), "v1.0.0", io.Discard); !errors.Is(err, storeFailure) {
		t.Fatal("failed read accepted", err)
	}
}

func TestGitHubAnswerSizeIsBounded(t *testing.T) {
	for _, size := range []int{releaseverify.MaxMetadata, releaseverify.MaxMetadata + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			value := `{"tag_name":"v2.0.0"}`
			body := value + strings.Repeat(" ", size-len(value))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
			defer server.Close()
			p := publication{api: server.URL, repository: "oberthci/oberth", client: server.Client()}
			err := p.finish(t.Context(), "v1.0.0", io.Discard)
			if (err == nil) != (size == releaseverify.MaxMetadata) {
				t.Fatalf("size %d: %v", size, err)
			}
		})
	}
}

func TestLatestInvalidHasRepairRemedy(t *testing.T) {
	p, fake := githubFixture(t)
	fake.add("nightly", false)
	fake.latest = "nightly"
	if err := p.finish(t.Context(), "v1.0.0", io.Discard); !errors.Is(err, latestInvalid) {
		t.Fatal("invalid latest not identified", err)
	}
}

func TestPublicationCancellation(t *testing.T) {
	p, _ := githubFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := p.finish(ctx, "v1.0.0", io.Discard); err == nil {
		t.Fatal("cancelled publication accepted")
	}
}

func TestGitHubRepositoryComesFromTheCanonicalOrigin(t *testing.T) {
	if repository, ok := githubRepository(releaseverify.Origin); !ok || repository != "oberthci/oberth" {
		t.Fatal("canonical origin names another repository", repository)
	}
	for _, origin := range []string{"http://github.com/oberthci/oberth/releases/download", "https://example.test/oberthci/oberth/releases/download",
		"https://github.com/oberthci/oberth/releases", "https://github.com/oberthci/oberth/releases/download/v1.0.0", "https://github.com//oberth/releases/download"} {
		if _, ok := githubRepository(origin); ok {
			t.Fatal("other origin accepted", origin)
		}
	}
}

func TestFinalizationRequiresExactCleanReceipt(t *testing.T) {
	o := options{dir: t.TempDir(), receipt: filepath.Join(t.TempDir(), "verified.json")}
	if err := os.WriteFile(filepath.Join(o.dir, "SHA256SUMS"), []byte("list"), 0644); err != nil {
		t.Fatal(err)
	}
	record := releaseverify.Record{Version: "v1.0.0", Commit: strings.Repeat("a", 40)}
	if err := verified(t.Context(), o, record); err == nil {
		t.Fatal("missing receipt accepted")
	}
	if err := writeVerification(t.Context(), o, record); err != nil {
		t.Fatal(err)
	}
	if err := verified(t.Context(), o, record); err != nil {
		t.Fatal(err)
	}
	record.Version = "v0.9.0"
	if err := verified(t.Context(), o, record); err == nil {
		t.Fatal("stale receipt accepted")
	}
	record.Version = "v1.0.0"
	record.Commit = strings.Repeat("b", 40)
	if err := verified(t.Context(), o, record); err == nil {
		t.Fatal("other commit accepted")
	}
	record.Commit = strings.Repeat("a", 40)
	if err := os.WriteFile(filepath.Join(o.dir, "SHA256SUMS"), []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := verified(t.Context(), o, record); err == nil {
		t.Fatal("changed checksum list accepted")
	}
}

type refusedClose struct{ io.Reader }

func (refusedClose) Close() error { return releaseverify.Digest }
func TestStagingReportsVerifierFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "staged")
	if err := stageFile(refusedClose{strings.NewReader("candidate bytes")}, path); !errors.Is(err, releaseverify.Digest) {
		t.Fatal("verification failure was lost during staging", err)
	}
}

func TestVersionFieldIsExact(t *testing.T) {
	for _, version := range []string{"v1.2.3", "v1.2.30", "prefix-v1.2.3"} {
		if versionMatches([]byte("oberth "+version+" commit=abc date=today\n"), "v1.2.3") != (version == "v1.2.3") {
			t.Fatal("inexact version", version)
		}
	}
}
