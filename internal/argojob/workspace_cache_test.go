package argojob

import (
	"encoding/json"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

// cacheLeafRequest is a pipeline with a "work" VCT and delivered artifacts,
// so every automatic workspace mount is in play, whose "main" leaf carries
// the given workspace annotations and a sibling leaf carries none.
func cacheLeafRequest(t *testing.T, trigger periapsis.Trigger, annotations map[string]string) Request {
	t.Helper()
	wf, err := argoworkflow.Decode([]byte(greedyDocument))
	if err != nil {
		t.Fatal(err)
	}
	wf.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "work"}, Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}}}}
	main := &wf.Spec.Templates[0]
	main.Container.Env = append(main.Container.Env,
		corev1.EnvVar{Name: "PATH", Value: "/private/bin"},
		corev1.EnvVar{Name: "GOCACHE", Value: "/tmp/private-gocache"},
		corev1.EnvVar{Name: "GOMODCACHE", Value: "/tmp/private-gomod"})
	sibling := main.DeepCopy()
	sibling.Name = "sibling"
	main.Metadata.Annotations = annotations
	wf.Spec.Templates = append(wf.Spec.Templates, *sibling)
	source, err := json.Marshal(wf)
	if err != nil {
		t.Fatal(err)
	}
	r := testRequest(trigger, string(source))
	r.SourceVolume = SourceVolume{ClaimName: "source-claim", SubPath: "source", ArtifactsSubPath: "artifacts"}
	return r
}

func templateNamed(t *testing.T, wf *wfv1.Workflow, name string) *wfv1.Template {
	t.Helper()
	for i := range wf.Spec.Templates {
		if wf.Spec.Templates[i].Name == name {
			return &wf.Spec.Templates[i]
		}
	}
	t.Fatalf("template %q not found", name)
	return nil
}

func cacheHostPath(t *testing.T, wf *wfv1.Workflow) string {
	t.Helper()
	for _, v := range wf.Spec.Volumes {
		if v.Name == CacheVolumeName {
			if v.HostPath == nil {
				t.Fatalf("cache volume is not a hostPath: %+v", v)
			}
			return v.HostPath.Path
		}
	}
	return ""
}

// A cache leaf keeps exactly the read-only source and the writable tier
// cache, and the three cache variables point into it -- with workspace-env
// absent or none. No work-VCT subpath, no artifacts mount, no tool/trivy
// defaults, no OBERTH_ARTIFACTS.
func TestWorkspaceCacheMountsOnlyTheCache(t *testing.T) {
	for name, annotations := range map[string]map[string]string{
		"cache":          {argoworkflow.WorkspaceMountsAnnotation: "cache"},
		"cache-env-none": {argoworkflow.WorkspaceMountsAnnotation: "cache", argoworkflow.WorkspaceEnvAnnotation: "none"},
	} {
		t.Run(name, func(t *testing.T) {
			wf, err := Build(cachingConfig(), cacheLeafRequest(t, periapsis.TriggerCI, annotations))
			if err != nil {
				t.Fatal(err)
			}
			leaf := templateNamed(t, wf, "main")
			var sawCache, sawSource bool
			for _, m := range leaf.Container.VolumeMounts {
				switch {
				case m.Name == CacheVolumeName:
					sawCache = true
					if m.MountPath != CacheMountPath || m.ReadOnly || m.SubPath != "" {
						t.Errorf("cache mount = %+v", m)
					}
				case m.Name == SourceVolumeName && m.MountPath == SourceMountPath:
					sawSource = true
					if !m.ReadOnly {
						t.Errorf("source became writable: %+v", m)
					}
				case m.Name == stepTmpVolumeName && m.MountPath == "/tmp":
					// The bounded per-Pod scratch every leaf gets.
				default:
					t.Errorf("cache leaf received unrequested mount %+v", m)
				}
			}
			if !sawCache || !sawSource {
				t.Fatalf("cache=%v source=%v mounts=%+v", sawCache, sawSource, leaf.Container.VolumeMounts)
			}
			env := environmentOf(t, wf, "main")
			if env["GOMODCACHE"] != ModuleCachePath || env["GOCACHE"] != BuildCachePath || env["OBERTH_CACHE_DIR"] != CacheMountPath {
				t.Fatalf("cache env = %v", env)
			}
			if env["PATH"] != "/private/bin" {
				t.Errorf("PATH = %q; authored value must survive", env["PATH"])
			}
			for _, absent := range []string{"OBERTH_ARTIFACTS", "OBERTH_TOOLS_DIR", "TRIVY_CACHE_DIR", "GOLANGCI_LINT_CACHE", "GOPATH"} {
				if value, ok := env[absent]; ok {
					t.Errorf("cache leaf received %s=%q", absent, value)
				}
			}
			if env["OBERTH_SHA"] != testSHA {
				t.Error("run identity missing")
			}
			// The sibling without controls keeps every default.
			siblingEnv := environmentOf(t, wf, "sibling")
			if siblingEnv["OBERTH_ARTIFACTS"] != ArtifactsMountPath || siblingEnv["OBERTH_TOOLS_DIR"] != WorkspaceToolsMountPath {
				t.Errorf("sibling lost defaults: %v", siblingEnv)
			}
		})
	}
}

// The trigger alone picks the tier root; a cache leaf on the release tier
// never sees the CI root, and the reverse.
func TestWorkspaceCacheStaysOnItsTier(t *testing.T) {
	annotations := map[string]string{argoworkflow.WorkspaceMountsAnnotation: "cache", argoworkflow.WorkspaceEnvAnnotation: "none"}
	for trigger, want := range map[periapsis.Trigger]string{
		periapsis.TriggerCI:      testCICacheRoot,
		periapsis.TriggerRelease: testReleaseCacheRoot,
	} {
		t.Run(string(trigger), func(t *testing.T) {
			r := cacheLeafRequest(t, trigger, annotations)
			r.Ref = "refs/tags/v1.2.3"
			wf, err := Build(cachingConfig(), r)
			if err != nil {
				t.Fatal(err)
			}
			got := cacheHostPath(t, wf)
			if !strings.HasPrefix(got, want+"/") {
				t.Fatalf("%s cache hostPath = %q; want under %s", trigger, got, want)
			}
			encoded, err := json.Marshal(wf)
			if err != nil {
				t.Fatal(err)
			}
			other := testReleaseCacheRoot
			if trigger == periapsis.TriggerRelease {
				other = testCICacheRoot
			}
			if strings.Contains(string(encoded), `"`+other) {
				t.Fatalf("%s workflow references the other tier root %s", trigger, other)
			}
		})
	}
	// A release tier with no cache root gets no cache, even when CI has one.
	config := cachingConfig()
	config.ReleaseCacheRoot = ""
	r := cacheLeafRequest(t, periapsis.TriggerRelease, annotations)
	r.Ref = "refs/tags/v1.2.3"
	wf, err := Build(config, r)
	if err != nil {
		t.Fatal(err)
	}
	if got := cacheHostPath(t, wf); got != "" {
		t.Fatalf("release run without a release root mounted %q", got)
	}
}

// With no root for the tier, a cache leaf has neither the persistent cache
// nor the per-run work-VCT fallback, and is never pointed at either path: the
// authored private locations stand.
func TestWorkspaceCacheWithoutTierRootKeepsAuthoredCaches(t *testing.T) {
	wf, err := Build(testConfig(), cacheLeafRequest(t, periapsis.TriggerCI, map[string]string{argoworkflow.WorkspaceMountsAnnotation: "cache"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range templateNamed(t, wf, "main").Container.VolumeMounts {
		if m.Name != SourceVolumeName && m.Name != stepTmpVolumeName {
			t.Errorf("unexpected mount %+v", m)
		}
	}
	env := environmentOf(t, wf, "main")
	if env["GOCACHE"] != "/tmp/private-gocache" || env["GOMODCACHE"] != "/tmp/private-gomod" {
		t.Fatalf("env = %v", env)
	}
	if _, ok := env["OBERTH_CACHE_DIR"]; ok {
		t.Fatal("OBERTH_CACHE_DIR advertised without a cache mount")
	}
}

// Credentialed release leaves may use cache: it is a strict subset of the
// default mounts, which those leaves already receive. Credential mounts are
// unaffected and the release-artifact work subpath is dropped.
func TestWorkspaceCacheOnCredentialedReleaseLeaf(t *testing.T) {
	r := isolationRequest(t, func(wf *wfv1.Workflow) {
		wf.Spec.Templates[0].Metadata.Annotations = map[string]string{argoworkflow.WorkspaceMountsAnnotation: "cache"}
	})
	wf, err := Build(cachingConfig(), r)
	if err != nil {
		t.Fatal(err)
	}
	if got := cacheHostPath(t, wf); !strings.HasPrefix(got, testReleaseCacheRoot+"/") {
		t.Fatalf("cache hostPath = %q", got)
	}
	var sawCache bool
	for _, m := range wf.Spec.Templates[0].Container.VolumeMounts {
		if m.Name == WorkspaceVolumeName || m.Name == ArtifactsVolumeName {
			t.Errorf("unrequested workspace mount %+v", m)
		}
		sawCache = sawCache || m.Name == CacheVolumeName
	}
	if !sawCache {
		t.Fatal("credentialed cache leaf lost its cache")
	}
	env := environmentOf(t, wf, "main")
	if env["GOCACHE"] != BuildCachePath || env["VAULT_ADDR"] != testVaultAddress {
		t.Fatalf("env = %v", env)
	}
}

// Like none, cache is a Pod-leaf control.
func TestWorkspaceCacheRefusedOnNonLeaf(t *testing.T) {
	r := isolationRequest(t, func(w *wfv1.Workflow) {
		leaf := w.Spec.Templates[0]
		leaf.Name = "leaf"
		w.Spec.Templates = []wfv1.Template{{
			Name:     "main",
			Metadata: wfv1.Metadata{Annotations: map[string]string{argoworkflow.WorkspaceMountsAnnotation: "cache"}},
			DAG:      &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "leaf", Template: "leaf"}}},
		}, leaf}
	})
	_, err := Build(testConfig(), r)
	if err == nil || !strings.Contains(err.Error(), "each Pod leaf") {
		t.Fatalf("err=%v", err)
	}
}

func trivyLeafWithMode(name, cacheDir, mode string) wfv1.Template {
	tmpl := trivyTemplate(name, cacheDir, false)
	if mode != "" {
		tmpl.Metadata.Annotations = map[string]string{argoworkflow.WorkspaceMountsAnnotation: mode}
	}
	return tmpl
}

// cache leaves do not receive the work-VCT trivy mount, so /tmp caches are
// private; but a --cache-dir inside the persistent cache is shared by every
// leaf that keeps it (cache, readonly, or default) and needs an edge.
func TestTrivyGuardWorkspaceCache(t *testing.T) {
	for _, test := range []struct {
		name, cacheDir, mode string
		refused              bool
	}{
		{"cache-private-tmp", WorkspaceTrivyCacheMountPath, "cache", false},
		{"cache-persistent", CacheMountPath + "/trivy", "cache", true},
		{"cache-persistent-root", CacheMountPath, "cache", true},
		{"cache-persistent-unclean", "/work/./cache//trivy/", "cache", true},
		{"cache-persistent-relative", "../cache/trivy", "cache", true},
		{"default-persistent", CacheMountPath + "/trivy", "", true},
		{"none-persistent-unmounted", CacheMountPath + "/trivy", "none", false},
		{"cache-sibling-path", "/work/cachefoo", "cache", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := trivyLeafWithMode("a", test.cacheDir, test.mode)
			b := trivyLeafWithMode("b", test.cacheDir, test.mode)
			a.Container.WorkingDir, b.Container.WorkingDir = SourceMountPath, SourceMountPath
			w := trivyWorkflow(true, trivyInlineDAGTask("a", a, ""), trivyInlineDAGTask("b", b, ""))
			err := trivyGuard(w)
			if (err != nil) != test.refused {
				t.Fatalf("refused=%v; want %v (err=%v)", err != nil, test.refused, err)
			}
		})
	}
}

// Every Pod container shape (script, containerSet, helpers with mirroring,
// inline DAG/steps leaves) gets the cache and nothing else automatic.
func TestWorkspaceCacheCoversEveryPodContainerShape(t *testing.T) {
	for _, shape := range []string{"container", "script", "containerset", "sidecars", "inline-dag", "inline-steps"} {
		t.Run(shape, func(t *testing.T) {
			r := isolationControllerRequest(t, shape)
			r.Source = []byte(strings.ReplaceAll(string(r.Source), `"oberth.ci/workspace-mounts":"readonly"`, `"oberth.ci/workspace-mounts":"cache"`))
			r.SourceVolume.ArtifactsSubPath = "artifacts"
			wf, err := Build(cachingConfig(), r)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			walkTemplates(wf, func(tmpl *wfv1.Template) {
				if workspaceMountMode(tmpl) != argoworkflow.WorkspaceMountsCache {
					return
				}
				templateContainers(tmpl, func(c *corev1.Container) {
					count++
					var cache bool
					for _, m := range c.VolumeMounts {
						cache = cache || (m.Name == CacheVolumeName && m.MountPath == CacheMountPath)
						if m.Name == ArtifactsVolumeName || (m.Name == WorkspaceVolumeName && strings.HasPrefix(m.MountPath, "/tmp/oberth-")) ||
							m.MountPath == WorkspaceGomodFallbackPath || m.MountPath == WorkspaceGobuildFallbackPath {
							t.Errorf("%s received automatic mount %+v", c.Name, m)
						}
					}
					if !cache {
						t.Errorf("%s lacks the cache mount", c.Name)
					}
					env := map[string]string{}
					for _, e := range c.Env {
						env[e.Name] = e.Value
					}
					if env["GOCACHE"] != BuildCachePath || env["GOMODCACHE"] != ModuleCachePath || env["PATH"] != "/private/bin" {
						t.Errorf("%s env = %v", c.Name, env)
					}
				})
			})
			if count == 0 {
				t.Fatal("fixture exercised no cache containers")
			}
		})
	}
}

// The CI cache root is written by every branch run, so a credentialed CI leaf
// must not link from it: a planted GOCACHE object would run with the leaf's
// secrets. Release leaves keep cache (only tag runs write that root), and a
// credentialed CI leaf may still use none.
func TestWorkspaceCacheRefusedOnCredentialedCILeaf(t *testing.T) {
	path := "oberth/upstream/oberthci/oberth/test-secret"
	request := func(trigger periapsis.Trigger, mode string) Request {
		document := strings.Replace(perRepoCredentialedDocument(path), "    - name: main\n",
			"    - name: main\n      metadata:\n        annotations:\n          oberth.ci/workspace-mounts: "+mode+"\n", 1)
		r := testRequest(trigger, document)
		r.Repo, r.UpstreamName, r.UpstreamOrg = "oberth", "codeberg", "oberthci"
		r.ApprovedSecrets = map[string]bool{path: true}
		r.SourceVolume = SourceVolume{ClaimName: "test-claim", SubPath: "src", VaultCASubPath: "vault-ca", BinarySubPath: "bin"}
		if trigger == periapsis.TriggerRelease {
			r.Ref = "refs/tags/v1.2.3"
		}
		return r
	}
	config := testConfigWithPerRepo()
	config.CICacheRoot, config.ReleaseCacheRoot = testCICacheRoot, testReleaseCacheRoot

	_, err := Build(config, request(periapsis.TriggerCI, "cache"))
	if err == nil || !strings.Contains(err.Error(), "cannot use oberth.ci/workspace-mounts: cache on the CI tier") {
		t.Fatalf("credentialed CI cache leaf: err=%v", err)
	}
	if _, err := Build(config, request(periapsis.TriggerCI, "none")); err != nil {
		t.Fatalf("credentialed CI none leaf refused: %v", err)
	}
	wf, err := Build(config, request(periapsis.TriggerRelease, "cache"))
	if err != nil {
		t.Fatalf("credentialed release cache leaf refused: %v", err)
	}
	if got := cacheHostPath(t, wf); !strings.HasPrefix(got, testReleaseCacheRoot+"/") {
		t.Fatalf("release cache hostPath = %q", got)
	}
}
