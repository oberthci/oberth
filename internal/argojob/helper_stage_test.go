package argojob

import (
	"bytes"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/nativebinding"
	core "k8s.io/api/core/v1"
)

func TestHoldStageVolumeFitsFullCheckout(t *testing.T) {
	t.Parallel()
	for _, script := range []bool{false, true} {
		leaf := wf.Template{Container: &core.Container{}}
		if script {
			leaf.Container, leaf.Script = nil, &wf.ScriptTemplate{}
		}
		if err := wrapHold(&leaf, nativebinding.Preparation{HelperImage: "fixture/helper:1"}); err != nil {
			t.Fatal(err)
		}
		assertStageVolumes(t, leaf)
		assertStageMounts(t, leaf)
	}
}

func holdEmptyDir(leaf wf.Template, name string) *core.EmptyDirVolumeSource {
	for _, v := range leaf.Volumes {
		if v.Name == name {
			return v.EmptyDir
		}
	}
	return nil
}

func assertStageVolumes(t *testing.T, leaf wf.Template) {
	t.Helper()
	stage, state := holdEmptyDir(leaf, "oberth-hold-stage"), holdEmptyDir(leaf, "oberth-hold-state")
	if stage == nil || stage.Medium != "" || stage.SizeLimit == nil || stage.SizeLimit.Value() < maxRerunArchive+maxRerunTree {
		t.Fatal("stage volume cannot hold archive and tree on disk", stage)
	}
	if state == nil || state.Medium != core.StorageMediumMemory || state.SizeLimit == nil || state.SizeLimit.Value() != 1<<20 {
		t.Fatal("socket state must stay on its small memory volume", state)
	}
}

func assertStageMounts(t *testing.T, leaf wf.Template) {
	t.Helper()
	mounts := 0
	for _, m := range holdMain(&leaf).VolumeMounts {
		if m.Name == "oberth-hold-stage" && m.MountPath == holdStage && !m.ReadOnly {
			mounts++
		}
	}
	if mounts != 1 || (holdRuntime{}).rerunStageRoot() != holdStage {
		t.Fatal("main and helper disagree about stage path")
	}
	for _, c := range append(leaf.InitContainers, leaf.Sidecars...) {
		for _, m := range c.VolumeMounts {
			if m.Name == "oberth-hold-stage" {
				t.Fatal("stage mounted outside main", c.Name)
			}
		}
	}
}

// Incompressible tracked data and its real Git objects both exceed the former
// 1 MiB state volume. The installed checkout must still be a clean F checkout.
func realisticRerunBundle(t *testing.T) ([]byte, string, string) {
	t.Helper()
	root := holdTempDir(t)
	git, _, _ := checkoutTestRepository(t, root)
	payload := make([]byte, 8<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tracked"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	git("commit", "-qam", "Full checkout")
	commit := git("rev-parse", "HEAD")
	files := checkoutTestFiles(t, root)
	files[".git/HEAD"] = []byte(commit + "\n")
	bundle, digest := rerunBundle(t, files, nil, commit)
	var treeBytes int
	for _, b := range files {
		treeBytes += len(b)
	}
	if len(bundle) <= 1<<20 || treeBytes <= 16<<20 {
		t.Fatal("checkout fixture is unrealistically small", len(bundle), treeBytes)
	}
	t.Logf("full checkout: %d archive bytes; %d tree bytes including Git objects", len(bundle), treeBytes)
	return bundle, digest, commit
}

func assertRealisticRerun(t *testing.T, root, commit string) {
	t.Helper()
	for _, args := range [][]string{{"rev-parse", "HEAD"}, {"status", "--porcelain"}, {"fsck", "--no-dangling"}} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil || args[0] == "rev-parse" && strings.TrimSpace(string(out)) != commit || args[0] == "status" && len(out) != 0 {
			t.Fatal("mounted full checkout differs from F", args, string(out), err)
		}
	}
}

func TestRerunStagesRealisticCheckoutOnDisk(t *testing.T) {
	t.Parallel()
	r := rerunRuntime(t)
	bundle, digest, commit := realisticRerunBundle(t)
	stage, err := r.stageRerun(t.Context(), bytes.NewReader(bundle), digest)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(stage)
	if filepath.Dir(stage) != r.stageRoot || filepath.Dir(stage) == r.root {
		t.Fatal("archive used socket state instead of disk stage", stage)
	}
	if _, err = r.applyRerunSnapshot(t.Context(), stage, digest); err != nil {
		t.Fatal(err)
	}
	assertRealisticRerun(t, r.sourceRoot, commit)
}
