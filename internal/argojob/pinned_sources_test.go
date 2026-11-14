package argojob

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestPinnedSourcesBuildRequiresServerBindingAndReadonlyMount(t *testing.T) {
	source := pipeline.PinnedSource{Repository: "forge/acme/producer", Commit: strings.Repeat("a", 40), Path: "/work/sources/producer"}
	body, _ := json.Marshal([]pipeline.PinnedSource{source})
	request := seededRequest(t, string(body))
	request.Source = []byte("sources: " + string(body) + "\n" + greedyDocument)
	if _, err := Build(testConfig(), request); err == nil {
		t.Fatal("unapproved source admitted")
	}
	request.PinnedSources = []PinnedSourceTree{{Source: source, Directory: t.TempDir()}}
	request.SourceVolume.ClaimName = sourceClaimName(request.Name)
	wf, err := Build(testConfig(), request)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for i := range wf.Spec.Templates {
		templateContainers(&wf.Spec.Templates[i], func(c *corev1.Container) {
			found := false
			for _, m := range c.VolumeMounts {
				if m.MountPath == source.Path {
					found = true
					count++
					if !m.ReadOnly || m.Name != SourceVolumeName || m.SubPath != "pinned-sources/0" {
						t.Fatalf("unsafe source mount: %+v", m)
					}
				}
			}
			if !found {
				t.Fatalf("container %s has no source", c.Name)
			}
			for _, e := range c.Env {
				if strings.Contains(e.Name, "TOKEN") || strings.Contains(e.Name, "PRIVATE_KEY") {
					t.Fatalf("source credential injected: %s", e.Name)
				}
			}
		})
	}
	if count == 0 {
		t.Fatal("no container inspected")
	}
	if err := verifyDeliveredPinnedSources(wf, []pipeline.PinnedSource{source}); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"writable", "wrong-subpath", "wrong-claim", "wrong-commit", "missing", "writable-alias", "parent-shadow", "expression-alias"} {
		t.Run(mode, func(t *testing.T) {
			live := wf.DeepCopy()
			for i := range live.Spec.Templates[0].Container.VolumeMounts {
				m := &live.Spec.Templates[0].Container.VolumeMounts[i]
				if m.MountPath == source.Path {
					switch mode {
					case "writable":
						m.ReadOnly = false
					case "wrong-subpath":
						m.SubPath = "src"
					case "missing":
						m.MountPath = "/missing"
					}
				}
			}
			if mode == "wrong-claim" {
				for i := range live.Spec.Volumes {
					if live.Spec.Volumes[i].Name == SourceVolumeName {
						live.Spec.Volumes[i].PersistentVolumeClaim.ClaimName = "other"
					}
				}
			}
			if mode == "wrong-commit" {
				live.Annotations[argoworkflow.SourcesAnnotation] = strings.ReplaceAll(live.Annotations[argoworkflow.SourcesAnnotation], source.Commit, strings.Repeat("c", 40))
			}
			if mode == "writable-alias" {
				live.Spec.Templates[0].Container.VolumeMounts = append(live.Spec.Templates[0].Container.VolumeMounts, corev1.VolumeMount{Name: SourceVolumeName, MountPath: "/alias", SubPath: "pinned-sources"})
			}
			if mode == "parent-shadow" {
				live.Spec.Templates[0].Container.VolumeMounts = append(live.Spec.Templates[0].Container.VolumeMounts, corev1.VolumeMount{Name: "other", MountPath: "/work/sources"})
			}
			if mode == "expression-alias" {
				live.Spec.Templates[0].Container.VolumeMounts = append(live.Spec.Templates[0].Container.VolumeMounts, corev1.VolumeMount{Name: SourceVolumeName, MountPath: "/alias", SubPathExpr: "$(PATH)"})
			}
			if err := verifyDeliveredPinnedSources(live, []pipeline.PinnedSource{source}); err == nil {
				t.Fatal("tampered source footprint verified")
			}
			if err := (&Controller{config: testConfig()}).sameCreateOwner(live, request); err == nil {
				t.Fatal("tampered source accepted on idempotent create")
			}
			if _, err := (&Controller{}).bindAdoption(live, AdoptionRequest{PinnedSources: []pipeline.PinnedSource{source}}); err == nil {
				t.Fatal("tampered source adopted")
			}
		})
	}
	changed := request
	changed.PinnedSources = append([]PinnedSourceTree(nil), request.PinnedSources...)
	changed.PinnedSources[0].Source.Commit = strings.Repeat("b", 40)
	if _, err := Build(testConfig(), changed); err == nil {
		t.Fatal("wrong source commit admitted")
	}
	decoded, err := argoworkflow.Decode(request.Source)
	if err != nil {
		t.Fatal(err)
	}
	decoded.Spec.Templates[0].Container.VolumeMounts = []corev1.VolumeMount{{Name: "scratch", MountPath: source.Path + "/child"}}
	raw, _ := json.Marshal(decoded)
	request.Source = raw
	if _, err := Build(testConfig(), request); err == nil {
		t.Fatal("shadow mount admitted")
	}
}

func TestPinnedSourceSeederStreamsExactTreeWithoutCredentials(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("exact contents"), 0644); err != nil {
		t.Fatal(err)
	}
	called := false
	seeder := &SourceSeeder{config: testConfig(), exec: func(_ context.Context, namespace, pod, container string, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
		called = true
		if strings.Join(command, " ") != "/bin/sh -c mkdir -p /seed/pinned-sources/0 && tar -xzf - -C /seed/pinned-sources/0" {
			t.Fatalf("untrusted extraction command: %v", command)
		}
		gz, err := gzip.NewReader(stdin)
		if err != nil {
			return err
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		header, err := tr.Next()
		if err != nil {
			return err
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		if header.Name != "file" || string(body) != "exact contents" {
			t.Fatalf("wrong payload: %s %q", header.Name, body)
		}
		if _, err := tr.Next(); err != io.EOF {
			t.Fatal("unexpected extra source entry")
		}
		_, err = io.Copy(io.Discard, gz)
		return err
	}}
	if err := seeder.fillPinnedSource(t.Context(), "seed", 0, dir); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("no source stream")
	}
}
