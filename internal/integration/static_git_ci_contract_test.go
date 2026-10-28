package integration_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/releaseimage"
	"sigs.k8s.io/yaml"
)

func checkStaticGitGate(workflow *wfv1.Workflow) error {
	templates := make(map[string]*wfv1.Template)
	for i := range workflow.Spec.Templates {
		template := &workflow.Spec.Templates[i]
		templates[template.Name] = template
	}
	build, runtime, ci := templates["build-static-git-runtime"], templates["test-static-git-runtime"], templates["ci"]
	if build == nil || runtime == nil || ci == nil || ci.DAG == nil || build.Container == nil || runtime.Container == nil {
		return fmt.Errorf("missing static Git gate")
	}
	if strings.Join(build.Container.Command, " ") != "/bin/sh" || strings.Join(build.Container.Args, " ") != "hack/qualify-static-git-build.sh" {
		return fmt.Errorf("canonical build bypassed")
	}
	if runtime.Container.Image != releaseimage.ServerSubstrate || strings.Join(runtime.Container.Command, " ") != "/qualification/gitcache.test" || strings.Contains(strings.Join(runtime.Container.Args, " "), "-test.run") || runtime.Container.WorkingDir != "/qualification/source/internal/gitcache" {
		return fmt.Errorf("qualification bypassed or narrowed")
	}
	wanted := map[string]string{"/qualification": "static-git-qualification", "/usr/bin/git": "static-git-qualification/git", "/usr/local/bin/oberth": "static-git-qualification/oberth"}
	for _, mount := range runtime.Container.VolumeMounts {
		if path, ok := wanted[mount.MountPath]; ok {
			if !mount.ReadOnly || mount.Name != "work" || mount.SubPath != path {
				return fmt.Errorf("qualification input is stale or writable")
			}
			delete(wanted, mount.MountPath)
		}
	}
	if len(wanted) != 0 {
		return fmt.Errorf("fresh artifacts not mounted")
	}
	seen := 0
	for _, task := range ci.DAG.Tasks {
		if task.Name == build.Name || task.Name == runtime.Name {
			expected := "setup"
			if task.Name == runtime.Name {
				expected = build.Name
			}
			if task.Template != task.Name || task.Depends != expected || task.When != "" || task.ContinueOn != nil {
				return fmt.Errorf("conditional or bypassed static Git gate")
			}
			seen++
		}
		switch task.Name {
		case "build-amd64", "build-arm64", "build-darwin-amd64", "build-darwin-arm64":
			if !strings.Contains(task.Depends, " && test-static-git-runtime && ") {
				return fmt.Errorf("release binary bypasses runtime gate")
			}
			seen++
		}
	}
	if seen != 6 {
		return fmt.Errorf("missing required dependency")
	}
	return nil
}

func TestStaticGitRuntimeIsARequiredFreshArtifactGate(t *testing.T) {
	body, err := os.ReadFile("../../.oberth/build.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow wfv1.Workflow
	if err := yaml.Unmarshal(body, &workflow); err != nil {
		t.Fatal(err)
	}
	if err := checkStaticGitGate(&workflow); err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"stale-git", "writable-helper", "conditional", "missing-gate"} {
		t.Run(fault, func(t *testing.T) {
			candidate := workflow.DeepCopy()
			for i := range candidate.Spec.Templates {
				template := &candidate.Spec.Templates[i]
				if template.Name == "test-static-git-runtime" {
					for j := range template.Container.VolumeMounts {
						mount := &template.Container.VolumeMounts[j]
						if fault == "stale-git" && mount.MountPath == "/usr/bin/git" {
							mount.SubPath = "old/git"
						}
						if fault == "writable-helper" && mount.MountPath == "/usr/local/bin/oberth" {
							mount.ReadOnly = false
						}
					}
				}
				if template.Name == "ci" {
					for j := range template.DAG.Tasks {
						task := &template.DAG.Tasks[j]
						if fault == "conditional" && task.Name == "test-static-git-runtime" {
							task.When = "false"
						}
						if fault == "missing-gate" && task.Name == "build-amd64" {
							task.Depends = "test-gitcache"
						}
					}
				}
			}
			if checkStaticGitGate(candidate) == nil {
				t.Fatal("unsafe gate accepted")
			}
		})
	}
	recipe, err := os.ReadFile("../../hack/qualify-static-git-build.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"hack/install-static-git-packages.sh", "hack/build-static-git.sh", "-tags=staticgit_qualification", "./cmd/oberth", "./hack/static-git-metadata", "hack/scan-static-git.sh"} {
		if !strings.Contains(string(recipe), required) {
			t.Fatalf("CI build omitted %s", required)
		}
	}
	release, err := os.ReadFile("../../.oberth/release.sh")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(release), "publish_images() {")
	end := strings.Index(string(release)[start:], "\nprepare_chart()")
	publish := string(release)[start : start+end]
	scan, sign := strings.Index(publish, "/bin/sh hack/scan-static-git.sh"), strings.Index(publish, "prepare_signing_identity")
	if scan < 0 || sign <= scan || !strings.Contains(publish[:scan], `"$release_image_tool" verify`) || !strings.Contains(publish, "amd64 arm64") {
		t.Fatal("C component scan/byte verification must precede image signing on both platforms")
	}
}
