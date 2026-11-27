package argojob

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func assertDiskStage(t *testing.T, path string) {
	t.Helper()
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil || fs.Type == unix.TMPFS_MAGIC {
		t.Fatal("fixture stage must be disk-backed", fs.Type, err)
	}
}

func TestRerunRefusesSmallStageBeforeReading(t *testing.T) {
	cmd := namespaceCommand(t, "small-stage")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		namespaceUnavailable(t, err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal("small stage refusal failed", out.String(), err)
	}
	t.Log(out.String())
}

func namespaceSmallStage(t *testing.T) {
	privateMounts(t)
	r := namespaceRuntime(t)
	if err := syscall.Mount("tmpfs", r.stageRoot, "tmpfs", 0, "size=1m,mode=0700"); err != nil {
		t.Fatal(err)
	}
	bundle, digest := rerunBundle(t, minimalRerunFiles(), nil, strings.Repeat("f", 40))
	assertSmallStageClient(t, r, bundle, digest)
	assertEmptyStageDirectory(t, r.root)
	assertEmptyStageDirectory(t, r.stageRoot)
	// Space may disappear after upload. Check again before extracting or
	// changing any source file, even for a previously verified stage.
	stage := filepath.Join(r.stageRoot, ".oberth-resume-fixture")
	if err := os.WriteFile(stage, bundle, 0600); err != nil {
		t.Fatal(err)
	}
	r.sourceInfo, _ = os.Lstat(r.sourceRoot)
	if _, err := r.applyRerunSnapshot(t.Context(), stage, digest); !errors.Is(err, errRerunStageSpace) {
		t.Fatal("small stage was not refused before extraction", err)
	}
	assertEmptyStageDirectory(t, r.sourceRoot)
}

func assertSmallStageClient(t *testing.T, r holdRuntime, bundle []byte, digest string) {
	t.Helper()
	input := bytes.NewReader(bundle)
	var diagnostic bytes.Buffer
	code := r.client(t.Context(), []string{"rerun", "--pod-uid", "uid", "--sha256", digest}, HoldIO{In: input, Out: io.Discard, Err: &diagnostic})
	frame, _, valid := holdReply(diagnostic.Bytes())
	if code != 120 || !valid || frame.Type != "refused" || frame.Reason != rerunStageReason || frame.Remedy != rerunStageRemedy || input.Len() != len(bundle) {
		t.Fatal("small stage was not refused before reading snapshot", code, diagnostic.String(), input.Len())
	}
}

func assertEmptyStageDirectory(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("refused stage changed state or checkout", entries, err)
	}
}
