package argojob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
)

func testResumeArchives(t *testing.T, p0, fix string) map[string][]byte {
	t.Helper()
	archives := map[string][]byte{}
	for _, commit := range []string{p0, fix} {
		files := minimalRerunFiles()
		files[".git/HEAD"] = []byte(commit + "\n")
		if commit == p0 {
			files["deleted"] = []byte("old")
		}
		bundle, _ := rerunBundle(t, files, nil, commit)
		reader := bytes.NewReader(bundle)
		var manifest rerunManifest
		if err := readHoldFrame(reader, &manifest); err != nil {
			t.Fatal(err)
		}
		archives[commit], _ = io.ReadAll(reader)
	}
	return archives
}

func TestResumeExportBindsFullSnapshotAndRemovedPaths(t *testing.T) {
	t.Parallel()
	p0, fix := strings.Repeat("a", 40), strings.Repeat("f", 40)
	archives := testResumeArchives(t, p0, fix)
	var staged []sourcecontract.Binding
	export := func(_ context.Context, b sourcecontract.Binding) (io.ReadCloser, error) {
		staged = append(staged, b)
		return io.NopCloser(bytes.NewReader(archives[b.Revision.Commit])), nil
	}
	binding := sourcecontract.Binding{Path: "/work/src", Descriptor: sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}, Revision: sourcecontract.Revision{Repository: "origin/team/repo", Commit: p0}}
	old := stateport.Run{Admission: admissioncontract.PreparedView{SourceBindings: []sourcecontract.Binding{binding}}}
	binding.Revision.Commit = fix
	fresh := admissioncontract.PreparedView{RunID: "r2", SourceBindings: []sourcecontract.Binding{binding}}
	e := New(Config{ResumeExport: export})
	defer e.queue.ShutDown()
	defer e.collector.queue.ShutDown()
	plan, err := e.PrepareResumeCheckout(t.Context(), old, fresh, ResumePlan{InPod: &nativebinding.ResumeHold{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.InPod.Removed) != 1 || plan.InPod.Removed[0] != "deleted" || plan.InPod.Source.Revision.Commit != fix {
		t.Fatal("export does not bind F and P0 removals", plan.InPod)
	}
	reader, err := resumeExport(t.Context(), export, *plan.InPod)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != plan.InPod.Digest {
		t.Fatal("audit digest differs from transferred bytes")
	}
	for _, b := range staged {
		if b.Descriptor.Kind != sourcecontract.Main {
			t.Fatal("pin exported for rerun")
		}
	}
	if !bytes.Contains(data, []byte("deleted")) {
		t.Fatal("removed paths not covered by transfer digest")
	}
}

func TestRerunSnapshotRejectsTamperingAndLinkAncestors(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"tamper", "symlink", "HEAD"} {
		t.Run(kind, func(t *testing.T) { checkTamperedSnapshot(t, kind) })
	}
}

func TestRerunClientRejectsWrongDigest(t *testing.T) {
	t.Parallel()
	r := rerunRuntime(t)
	bundle, _ := rerunBundle(t, minimalRerunFiles(), nil, strings.Repeat("f", 40))
	stage, err := r.stageRerun(t.Context(), bytes.NewReader(bundle), strings.Repeat("0", 64))
	if stage != "" {
		_ = os.Remove(stage)
	}
	if err == nil {
		t.Fatal("client accepted mismatched snapshot digest")
	}
}

func checkTamperedSnapshot(t *testing.T, kind string) {
	t.Helper()
	r := rerunRuntime(t)
	files := minimalRerunFiles()
	files["nested/output"] = []byte("F")
	if kind == "HEAD" {
		files[".git/HEAD"] = []byte(strings.Repeat("a", 40) + "\n")
	}
	bundle, digest := rerunBundle(t, files, nil, strings.Repeat("f", 40))
	stage, err := r.stageRerun(t.Context(), bytes.NewReader(bundle), digest)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(stage)
	outside := t.TempDir()
	if kind == "tamper" {
		files["FIXED"] = []byte("tampered")
		changed, _ := rerunBundle(t, files, nil, strings.Repeat("f", 40))
		if err = os.WriteFile(stage, changed, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if kind == "symlink" {
		if err = os.Symlink(outside, filepath.Join(r.sourceRoot, "nested")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = r.applyRerunSnapshot(t.Context(), stage, digest); err == nil {
		t.Fatal("unsafe snapshot installed", kind)
	}
	if _, err = os.Stat(filepath.Join(outside, "output")); !os.IsNotExist(err) {
		t.Fatal("escaped checkout", err)
	}
}
