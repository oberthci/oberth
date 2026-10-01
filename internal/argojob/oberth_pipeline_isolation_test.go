package argojob

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

// Issue #567: a credentialed release step must never execute a binary from a
// volume that an earlier step running third-party code could write, and no
// step that needs no credential may hold one. These tests hold Oberth's own
// .oberth/build.yaml and .oberth/release.yaml to that on the Pods the real
// admission chain produces -- with a persistent cache root and a delivered
// artifacts claim configured, so a leaf that forgot to opt out would visibly
// receive the shared /work/cache hostPath, /work/artifacts and the Vault
// coordinates.

// oberthReleaseCredentialedLeaves are the only leaves that run a credential
// chain. Each one verifies its executables in an init container before the
// secretstore in its main container can fetch anything.
var oberthReleaseCredentialedLeaves = []string{
	"release-publish-chart",
	"release-publish-homebrew",
	"release-publish-images",
	"release-sign-binaries",
	"release-verify",
	"release-website",
}

// Setup steps that populate the trusted publisher tools. No other release
// leaf may mount bootstrap-tools writable.
var oberthReleaseToolWriters = map[string]bool{
	"tools-dir": true, "copy-trivy": true, "link-go": true, "install-golangci-lint": true,
	"install-helm": true, "fetch-cosign": true, "install-cosign": true,
	"build-release-support": true, "build-release-image": true,
}

// Leaves that run repository tests, linters, scanners or downloaded release
// artifacts. They receive their own read-only tool copies, never the trusted
// publisher tools.
var oberthReleaseTestLeaves = map[string]bool{
	"vet": true, "golangci-lint-run": true, "release-test": true, "release-chart-test": true,
	"release-build": true, "release-runtime": true, "release-scan": true,
}

// The build pipeline's setup burn populates the per-run tools; the Go leaves
// keep the persistent Go cache (read-write) but see the tools read-only; every
// other leaf opts out of the shared workspace entirely.
var (
	oberthBuildToolWriters = map[string]bool{
		"tools-dir": true, "link-go": true, "install-golangci-lint": true, "install-helm": true,
		"fetch-shellcheck": true, "verify-shellcheck": true, "decompress-shellcheck": true, "install-shellcheck": true,
	}
	oberthBuildCachedGoLeaves = map[string]bool{
		"vet": true, "golangci-lint-run": true, "test": true,
		"build-amd64": true, "build-arm64": true,
		"build-darwin-amd64": true, "build-darwin-arm64": true,
	}
	oberthBuildIsolatedLeaves = map[string]bool{
		"shellcheck": true, "chart": true, "security": true, "release-diagnostic": true,
	}
)

func buildOberthPipelineForIsolation(t *testing.T, trigger periapsis.Trigger, source []byte) (*wfv1.Workflow, error) {
	t.Helper()
	config := oberthConfig()
	config.CICacheRoot = "/var/lib/oberth-cache/ci"
	config.ReleaseCacheRoot = "/var/lib/oberth-cache/release"
	approved := map[string]bool{}
	ref := "refs/heads/main"
	if trigger == periapsis.TriggerRelease {
		approved = oberthReleaseApprovedSecrets()
		ref = "refs/tags/v0.1.0"
	}
	return Build(config, Request{
		RunID: "run-abc123", Name: "oberth-oberth-run-abc1-aabbccddeeff",
		Repo: "oberth", UpstreamOrg: "skipops",
		Ref: ref, SHA: testSHA, Trigger: trigger,
		Source: source, ApprovedSecrets: approved,
		SourceVolume: SourceVolume{
			ClaimName: "oberth-source-run", SubPath: "run/src", VaultCASubPath: "run/ca",
			ArtifactsSubPath: "run/artifacts", BinarySubPath: "run/bin",
		},
	})
}

func isolationLeaf(template *wfv1.Template) bool {
	return template.Container != nil || template.Script != nil || template.ContainerSet != nil
}

func isolationContainers(template *wfv1.Template) []*corev1.Container {
	var out []*corev1.Container
	templateContainers(template, func(c *corev1.Container) { out = append(out, c) })
	return out
}

func serverSharedMount(mount corev1.VolumeMount) bool {
	return mount.Name == WorkspaceVolumeName || mount.Name == CacheVolumeName || mount.Name == ArtifactsVolumeName
}

func credentialMaterial(template *wfv1.Template) error {
	for _, c := range isolationContainers(template) {
		for _, m := range c.VolumeMounts {
			if m.Name == ReleaseTokenVolumeName || m.Name == SecretsVolumeName ||
				m.MountPath == OberthBinMountPath || m.MountPath == VaultCAMountPath {
				return fmt.Errorf("mounts %s at %s", m.Name, m.MountPath)
			}
		}
		for _, e := range c.Env {
			if strings.HasPrefix(e.Name, "VAULT_") || e.Name == "OBERTH_VAULT_ROLE" {
				return fmt.Errorf("receives %s", e.Name)
			}
		}
	}
	return nil
}

func isolatedAnnotations(template *wfv1.Template) bool {
	return template.Metadata.Annotations[argoworkflow.WorkspaceMountsAnnotation] == "none" &&
		template.Metadata.Annotations[argoworkflow.WorkspaceEnvAnnotation] == "none"
}

func tokenless(template *wfv1.Template) bool {
	return template.AutomountServiceAccountToken != nil && !*template.AutomountServiceAccountToken
}

// trivyScanCache returns the cache directory of a Trivy scan leaf, or "" when
// the leaf runs no scan (copying the trivy binary is not a scan).
func trivyScanCache(template *wfv1.Template) string {
	subcommands := map[string]bool{"fs": true, "filesystem": true, "rootfs": true, "image": true, "repo": true, "repository": true, "sbom": true, "vm": true, "config": true}
	for _, c := range isolationContainers(template) {
		words := append(append([]string{}, c.Command...), c.Args...)
		for i, word := range words {
			if (word == "trivy" || strings.HasSuffix(word, "/trivy")) && i+1 < len(words) && subcommands[words[i+1]] {
				for j := i + 2; j+1 < len(words); j++ {
					if words[j] == "--cache-dir" {
						return words[j+1]
					}
				}
				return WorkspaceTrivyCacheMountPath
			}
		}
	}
	return ""
}

// trivyCacheBacked reports whether a Trivy scan keeps its database on a volume
// another Pod can reach (#655). The per-Pod /tmp emptyDir is private.
func trivyCacheBacked(template *wfv1.Template, cache string) bool {
	for _, c := range isolationContainers(template) {
		for _, m := range c.VolumeMounts {
			if m.Name == stepTmpVolumeName {
				continue
			}
			root := strings.TrimSuffix(m.MountPath, "/")
			if cache == root || strings.HasPrefix(cache, root+"/") {
				return true
			}
		}
	}
	return false
}

var (
	credentialedPathPattern = regexp.MustCompile(`exec /usr/bin/env -i PATH=(\S+) `)
	pathAssignmentPattern   = regexp.MustCompile(`(^|\s)PATH=`)
)

func checkOberthReleaseIsolation(workflow *wfv1.Workflow) error {
	var problems []string
	var credentialed []string
	walkTemplates(workflow, func(template *wfv1.Template) {
		if !isolationLeaf(template) {
			return
		}
		name := template.Name
		fail := func(format string, args ...any) { problems = append(problems, name+": "+fmt.Sprintf(format, args...)) }
		if !isolatedAnnotations(template) {
			fail("does not declare workspace-mounts none and workspace-env none")
		}
		for _, c := range isolationContainers(template) {
			for _, m := range c.VolumeMounts {
				if serverSharedMount(m) {
					fail("container %q receives shared %s at %s", c.Name, m.Name, m.MountPath)
				}
				if m.Name == "bootstrap-tools" && !m.ReadOnly && !oberthReleaseToolWriters[name] {
					fail("container %q can write the trusted publisher tools", c.Name)
				}
				if m.Name == "bootstrap-tools" && oberthReleaseTestLeaves[name] {
					fail("test leaf reaches the trusted publisher tools")
				}
				if m.Name == "bootstrap-test-tools" && !m.ReadOnly && name != "copy-test-tools" {
					fail("container %q can write test tools", c.Name)
				}
			}
		}
		if cache := trivyScanCache(template); cache != "" && trivyCacheBacked(template, cache) {
			fail("Trivy database is on a shared volume")
		}
		if templateUsesCredentialChain(template) {
			credentialed = append(credentialed, name)
			if tokenless(template) {
				fail("credentialed leaf disables its token")
			}
			if len(template.InitContainers) == 0 {
				fail("credentialed leaf has no pre-secret verifier")
				return
			}
			first := strings.Join(append(append([]string{}, template.InitContainers[0].Command...), template.InitContainers[0].Args...), " ")
			if !strings.Contains(first, "/usr/bin/python3 -I -S -B /work/src/.oberth/verify-") {
				fail("first init container is not the admitted-source verifier")
			}
			if template.Container == nil {
				fail("credentialed leaf is not a container")
				return
			}
			child := strings.Join(template.Container.Args, " ")
			match := credentialedPathPattern.FindStringSubmatch(child)
			if match == nil || match[1] != "/usr/bin:/bin" || len(pathAssignmentPattern.FindAllString(child, -1)) != 1 {
				fail("credentialed shell can resolve tools through a writable PATH")
			}
			return
		}
		if !tokenless(template) {
			fail("non-credentialed leaf does not declare automountServiceAccountToken: false")
		}
		if err := credentialMaterial(template); err != nil {
			fail("non-credentialed leaf %v", err)
		}
	})
	sort.Strings(credentialed)
	if strings.Join(credentialed, ",") != strings.Join(oberthReleaseCredentialedLeaves, ",") {
		problems = append(problems, fmt.Sprintf("credentialed leaves = %v, want %v", credentialed, oberthReleaseCredentialedLeaves))
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "\n"))
	}
	return nil
}

func checkOberthBuildIsolation(workflow *wfv1.Workflow) error {
	var problems []string
	seen := map[string]bool{}
	walkTemplates(workflow, func(template *wfv1.Template) {
		if !isolationLeaf(template) {
			return
		}
		name := template.Name
		seen[name] = true
		fail := func(format string, args ...any) { problems = append(problems, name+": "+fmt.Sprintf(format, args...)) }
		if !tokenless(template) {
			fail("does not declare automountServiceAccountToken: false")
		}
		if templateUsesCredentialChain(template) {
			fail("branch pipeline runs a credential chain")
		}
		if cache := trivyScanCache(template); cache != "" && trivyCacheBacked(template, cache) {
			fail("Trivy database is on a shared volume")
		}
		switch {
		case oberthBuildToolWriters[name]:
			return
		case oberthBuildCachedGoLeaves[name]:
			tools := false
			for _, c := range isolationContainers(template) {
				for _, m := range c.VolumeMounts {
					if m.Name == WorkspaceVolumeName && m.MountPath == WorkspaceToolsMountPath {
						tools = true
						if !m.ReadOnly {
							fail("container %q can write the setup-installed tools", c.Name)
						}
					}
					if m.Name == WorkspaceVolumeName && m.SubPath == "" {
						fail("container %q mounts the whole work claim", c.Name)
					}
				}
				for _, e := range c.Env {
					if e.Name == "GOLANGCI_LINT_CACHE" && strings.HasPrefix(e.Value, WorkspaceToolsMountPath) && name == "golangci-lint-run" &&
						!strings.HasPrefix(strings.Join(c.Args, " "), "GOLANGCI_LINT_CACHE=/tmp/private-") {
						fail("golangci-lint writes its cache inside the read-only tools")
					}
				}
			}
			if !tools {
				fail("Go leaf lost its read-only tools mount")
			}
		case oberthBuildIsolatedLeaves[name]:
			if !isolatedAnnotations(template) {
				fail("does not declare workspace-mounts none and workspace-env none")
			}
			for _, c := range isolationContainers(template) {
				for _, m := range c.VolumeMounts {
					if m.Name == CacheVolumeName || m.Name == ArtifactsVolumeName {
						fail("container %q receives shared %s", c.Name, m.Name)
					}
					if m.Name == WorkspaceVolumeName && (m.SubPath == "" || (m.SubPath == "tools" && !m.ReadOnly)) {
						fail("container %q can write the work claim's tools", c.Name)
					}
				}
			}
		default:
			fail("leaf has no declared workspace-isolation class")
		}
	})
	for name := range oberthBuildIsolatedLeaves {
		if !seen[name] {
			problems = append(problems, name+": expected leaf missing")
		}
	}
	for name := range oberthBuildCachedGoLeaves {
		if !seen[name] {
			problems = append(problems, name+": expected leaf missing")
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "\n"))
	}
	return nil
}

// TestOberthPipelinesIsolateSharedWorkspace proves #567 on the admitted Pods.
func TestOberthPipelinesIsolateSharedWorkspace(t *testing.T) {
	release, err := buildOberthPipelineForIsolation(t, periapsis.TriggerRelease, loadPipeline(t, "release.yaml"))
	if err != nil {
		t.Fatalf("release admission: %v", err)
	}
	if err := checkOberthReleaseIsolation(release); err != nil {
		t.Errorf("release.yaml:\n%v", err)
	}
	build, err := buildOberthPipelineForIsolation(t, periapsis.TriggerCI, loadPipeline(t, "build.yaml"))
	if err != nil {
		t.Fatalf("build admission: %v", err)
	}
	if err := checkOberthBuildIsolation(build); err != nil {
		t.Errorf("build.yaml:\n%v", err)
	}
}

// TestOberthPipelinesIsolationRejectsDrift proves each rule catches the
// regression it names; a mutation Oberth's own admission refuses also counts.
func TestOberthPipelinesIsolationRejectsDrift(t *testing.T) {
	cases := []struct {
		file, name, old, replacement string
	}{
		{"release.yaml", "test leaf takes the shared workspace",
			"  - name: release-test\n    automountServiceAccountToken: false\n    metadata:\n      annotations:\n        oberth.ci/workspace-mounts: none\n",
			"  - name: release-test\n    automountServiceAccountToken: false\n    metadata:\n      annotations:\n"},
		{"release.yaml", "scanner keeps the Vault coordinates",
			"  - name: release-scan\n    automountServiceAccountToken: false\n", "  - name: release-scan\n"},
		{"release.yaml", "publisher loses its verifier",
			"    initContainers:\n    - name: verify-publisher-tools\n", "    sidecars:\n    - name: verify-publisher-tools\n"},
		{"release.yaml", "test leaf mounts trusted tools",
			"      - name: bootstrap-test-tools\n        mountPath: /tmp/oberth-tools\n        subPath: release-test\n",
			"      - name: bootstrap-tools\n        mountPath: /tmp/oberth-tools\n"},
		{"build.yaml", "Go leaf can write the tools",
			"        subPath: tools\n        readOnly: true\n      - name: work\n        mountPath: /cache/gomod\n",
			"        subPath: tools\n      - name: work\n        mountPath: /cache/gomod\n"},
		{"build.yaml", "scanner shares the work claim",
			"  - name: security\n    automountServiceAccountToken: false\n    metadata:\n      annotations:\n        oberth.ci/workspace-mounts: none\n        oberth.ci/workspace-env: none\n",
			"  - name: security\n    automountServiceAccountToken: false\n"},
		{"build.yaml", "diagnostic mounts the whole claim",
			"        mountPath: /tmp/diagnostic-work\n        subPath: diagnostic\n", "        mountPath: /tmp/diagnostic-work\n"},
		{"build.yaml", "lint cache inside the read-only tools",
			`args: ["GOLANGCI_LINT_CACHE=/tmp/private-golangci-lint", "/tmp/oberth-tools/bin/golangci-lint", `,
			`args: ["/tmp/oberth-tools/bin/golangci-lint", `},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := string(loadPipeline(t, tc.file))
			if strings.Count(raw, tc.old) < 1 {
				t.Fatalf("mutation anchor no longer matches %q", tc.old)
			}
			mutated := strings.Replace(raw, tc.old, tc.replacement, 1)
			trigger := periapsis.TriggerCI
			check := checkOberthBuildIsolation
			if tc.file == "release.yaml" {
				trigger = periapsis.TriggerRelease
				check = checkOberthReleaseIsolation
			}
			workflow, err := buildOberthPipelineForIsolation(t, trigger, []byte(mutated))
			if err != nil {
				return // admission itself refused the drift
			}
			if err := check(workflow); err == nil {
				t.Fatalf("drift %q was accepted", tc.name)
			}
		})
	}
}
