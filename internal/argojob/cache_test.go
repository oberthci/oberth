package argojob

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/pkg/periapsis"
)

const (
	testCICacheRoot      = "/var/cache/oberth/ci"
	testReleaseCacheRoot = "/var/cache/oberth/release"
)

func cachingConfig() Config {
	config := testConfig()
	config.CICacheRoot = testCICacheRoot
	config.ReleaseCacheRoot = testReleaseCacheRoot
	return config
}

// greedyCacheDocument declares its own Go cache environment and its own volume
// mount at the server's cache path. Both are things a repository must not be
// able to decide.
const greedyCacheDocument = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/size: M
spec:
  entrypoint: main
  activeDeadlineSeconds: 600
  templates:
    - name: main
      container:
        image: golang:1.26-trixie@sha256:ab563819a16cfe5faff0f96a8bb598fbb0e400ab2ac751996e60abcb23b106a3
        command: [/bin/true]
        env:
          - name: GOMODCACHE
            value: /somewhere/else
          - name: GOCACHE
            value: /somewhere/else/build
          - name: KEEP_ME
            value: yes-please
        volumeMounts:
          - name: scratch
            mountPath: /work/cache
`

func findVolume(t *testing.T, workflow *wfv1.Workflow, name string) corev1.Volume {
	t.Helper()
	for _, volume := range workflow.Spec.Volumes {
		if volume.Name == name {
			return volume
		}
	}
	t.Fatalf("workflow declares no volume named %q; volumes: %+v", name, workflow.Spec.Volumes)
	return corev1.Volume{}
}

func mainContainer(t *testing.T, workflow *wfv1.Workflow) *corev1.Container {
	t.Helper()
	for index := range workflow.Spec.Templates {
		if workflow.Spec.Templates[index].Container != nil {
			return workflow.Spec.Templates[index].Container
		}
	}
	t.Fatal("workflow declares no container template")
	return nil
}

func environmentValue(container *corev1.Container, name string) (string, bool) {
	for _, variable := range container.Env {
		if variable.Name == name {
			return variable.Value, true
		}
	}
	return "", false
}

func mountPaths(container *corev1.Container, volumeName string) []string {
	var paths []string
	for _, mount := range container.VolumeMounts {
		if mount.Name == volumeName {
			paths = append(paths, mount.MountPath)
		}
	}
	return paths
}

// TestBuildInjectsThePersistentCache is the plain statement of the feature: a
// branch run receives a writable node-local directory scoped to its repository,
// and is told where it is.
func TestBuildInjectsThePersistentCache(t *testing.T) {
	workflow, err := Build(cachingConfig(), testRequest(periapsis.TriggerCI, greedyCacheDocument))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	volume := findVolume(t, workflow, CacheVolumeName)
	if volume.HostPath == nil {
		t.Fatalf("cache volume is not a hostPath: %+v", volume.VolumeSource)
	}
	if !strings.HasPrefix(volume.HostPath.Path, testCICacheRoot+"/") {
		t.Fatalf("cache path %q is not under the CI tier root %q", volume.HostPath.Path, testCICacheRoot)
	}
	if volume.HostPath.Type == nil || *volume.HostPath.Type != corev1.HostPathDirectoryOrCreate {
		t.Fatalf("cache hostPath type = %v, want DirectoryOrCreate", volume.HostPath.Type)
	}
	container := mainContainer(t, workflow)
	if paths := mountPaths(container, CacheVolumeName); len(paths) != 1 || paths[0] != CacheMountPath {
		t.Fatalf("cache mount paths = %v, want exactly [%s]", paths, CacheMountPath)
	}
	for name, want := range map[string]string{
		"GOMODCACHE":       ModuleCachePath,
		"GOCACHE":          BuildCachePath,
		"OBERTH_CACHE_DIR": CacheMountPath,
	} {
		got, ok := environmentValue(container, name)
		if !ok {
			t.Fatalf("container does not receive %s", name)
		}
		if got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	// The cache must not cost the document its own unrelated environment.
	if value, ok := environmentValue(container, "KEEP_ME"); !ok || value != "yes-please" {
		t.Fatalf("KEEP_ME = %q (present=%v), want the document's own value", value, ok)
	}
}

// TestBuildKeepsTheCacheTiersApart is the security property. A cache is writable
// state that outlives the run that wrote it, so it is the one place a branch
// build could hand bytes to a signed release. The two tiers must never resolve
// to the same directory, and neither may sit inside the other.
func TestBuildKeepsTheCacheTiersApart(t *testing.T) {
	config := cachingConfig()
	ciWorkflow, err := Build(config, testRequest(periapsis.TriggerCI, greedyCacheDocument))
	if err != nil {
		t.Fatalf("Build(CI): %v", err)
	}
	releaseWorkflow, err := Build(config, testRequest(periapsis.TriggerRelease, greedyCacheDocument))
	if err != nil {
		t.Fatalf("Build(release): %v", err)
	}
	ciPath := findVolume(t, ciWorkflow, CacheVolumeName).HostPath.Path
	releasePath := findVolume(t, releaseWorkflow, CacheVolumeName).HostPath.Path

	if ciPath == releasePath {
		t.Fatalf("both tiers share the cache directory %q", ciPath)
	}
	if strings.HasPrefix(ciPath+"/", releasePath+"/") || strings.HasPrefix(releasePath+"/", ciPath+"/") {
		t.Fatalf("one tier's cache is nested inside the other: CI=%q release=%q", ciPath, releasePath)
	}
	if !strings.HasPrefix(ciPath+"/", testCICacheRoot+"/") {
		t.Fatalf("CI cache %q escaped the CI root", ciPath)
	}
	if !strings.HasPrefix(releasePath+"/", testReleaseCacheRoot+"/") {
		t.Fatalf("release cache %q escaped the release root", releasePath)
	}
	// The same repository on both tiers must still land on the same *name*
	// inside its own root, so a repository's cache is per repository, not
	// per run.
	if strings.TrimPrefix(ciPath, testCICacheRoot) != strings.TrimPrefix(releasePath, testReleaseCacheRoot) {
		t.Fatalf("the same repository derived different cache names: %q vs %q", ciPath, releasePath)
	}
}

// TestBuildSeparatesCachesByRepository keeps one repository's build outputs out
// of another's, which is what makes a shared node safe to cache on at all.
func TestBuildSeparatesCachesByRepository(t *testing.T) {
	config := cachingConfig()
	first := testRequest(periapsis.TriggerCI, greedyCacheDocument)
	second := testRequest(periapsis.TriggerCI, greedyCacheDocument)
	second.Repo = "acme-wrapper"

	firstWorkflow, err := Build(config, first)
	if err != nil {
		t.Fatalf("Build(first): %v", err)
	}
	secondWorkflow, err := Build(config, second)
	if err != nil {
		t.Fatalf("Build(second): %v", err)
	}
	firstPath := findVolume(t, firstWorkflow, CacheVolumeName).HostPath.Path
	secondPath := findVolume(t, secondWorkflow, CacheVolumeName).HostPath.Path
	if firstPath == secondPath {
		t.Fatalf("two repositories share the cache directory %q", firstPath)
	}
}

// TestBuildOmitsTheCacheWhenTheTierHasNoRoot proves the feature is optional and
// fails open into "slow", never into "broken".
func TestBuildOmitsTheCacheWhenTheTierHasNoRoot(t *testing.T) {
	workflow, err := Build(testConfig(), testRequest(periapsis.TriggerCI, greedyCacheDocument))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, volume := range workflow.Spec.Volumes {
		if volume.Name == CacheVolumeName {
			t.Fatal("an unconfigured tier still received a cache volume")
		}
	}
	container := mainContainer(t, workflow)
	// Without a server cache the document's own value survives, because the
	// server has nothing to say about where the compiler should cache.
	if value, _ := environmentValue(container, "GOMODCACHE"); value != "/somewhere/else" {
		t.Fatalf("GOMODCACHE = %q, want the document's own value when no cache is configured", value)
	}
	if _, ok := environmentValue(container, "OBERTH_CACHE_DIR"); ok {
		t.Fatal("OBERTH_CACHE_DIR advertised without a cache mount")
	}
}

// TestBuildStripsARepositoryMountAtTheCachePath proves the server owns the path,
// not just the volume name: a document mounting anything at /work/cache loses it.
func TestBuildStripsARepositoryMountAtTheCachePath(t *testing.T) {
	workflow, err := Build(cachingConfig(), testRequest(periapsis.TriggerCI, greedyCacheDocument))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	container := mainContainer(t, workflow)
	for _, mount := range container.VolumeMounts {
		if mount.MountPath == CacheMountPath && mount.Name != CacheVolumeName {
			t.Fatalf("document mount %q survived at the server-owned cache path", mount.Name)
		}
	}
}

// TestBuildKeepsTheSourceMountAlongsideTheCache is the regression guard for the
// single-walk invariant: the cache and the checkout are attached in one pass, so
// adding one must not strip the other.
func TestBuildKeepsTheSourceMountAlongsideTheCache(t *testing.T) {
	request := testRequest(periapsis.TriggerCI, greedyCacheDocument)
	request.SourceVolume = SourceVolume{ClaimName: "run-claim", SubPath: "src"}
	workflow, err := Build(cachingConfig(), request)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	container := mainContainer(t, workflow)
	if paths := mountPaths(container, SourceVolumeName); len(paths) != 1 || paths[0] != SourceMountPath {
		t.Fatalf("source mount paths = %v, want exactly [%s]", paths, SourceMountPath)
	}
	if paths := mountPaths(container, CacheVolumeName); len(paths) != 1 || paths[0] != CacheMountPath {
		t.Fatalf("cache mount paths = %v, want exactly [%s]", paths, CacheMountPath)
	}
}

// TestRepoCacheSegmentIsAContainedSingleSegment is the path-traversal guard. The
// repository name reaches this function straight from a push, and its output is
// concatenated into a node path.
func TestRepoCacheSegmentIsAContainedSingleSegment(t *testing.T) {
	hostile := []string{
		"..", ".", "../..", "../../etc/shadow", "a/b/c", "/absolute",
		"", "   ", "....", "..%2f..", "repo\x00name", "REPO..Name",
		strings.Repeat("x", 512), strings.Repeat("../", 64),
	}
	seen := map[string]string{}
	for _, repo := range hostile {
		segment := repoCacheSegment(repo, "testorg")
		if strings.ContainsAny(segment, "/\x00") {
			t.Fatalf("repo %q produced a segment with a separator: %q", repo, segment)
		}
		if strings.Contains(segment, "..") || segment == "." {
			t.Fatalf("repo %q produced a traversal segment: %q", repo, segment)
		}
		if segment == "" {
			t.Fatalf("repo %q produced an empty segment", repo)
		}
		// Joining must stay inside the root for every one of them.
		joined := testCICacheRoot + "/" + segment
		if !strings.HasPrefix(joined, testCICacheRoot+"/") || strings.Contains(joined, "/../") {
			t.Fatalf("repo %q escaped the root: %q", repo, joined)
		}
		if previous, clash := seen[segment]; clash {
			t.Fatalf("repos %q and %q collided on segment %q", previous, repo, segment)
		}
		seen[segment] = repo
	}
}

// TestValidateRefusesUnsafeCacheRoots keeps a hand-run server from being
// configured into the shape the chart's own validations refuse to render.
func TestValidateRefusesUnsafeCacheRoots(t *testing.T) {
	cases := map[string]struct {
		ci, release string
		wantErr     bool
	}{
		"distinct absolute roots":  {"/var/cache/oberth/ci", "/var/cache/oberth/release", false},
		"both empty":               {"", "", false},
		"only CI configured":       {"/var/cache/oberth/ci", "", false},
		"identical roots":          {"/var/cache/oberth", "/var/cache/oberth", true},
		"release nested inside CI": {"/var/cache/oberth", "/var/cache/oberth/release", true},
		"CI nested inside release": {"/var/cache/oberth/ci", "/var/cache/oberth", true},
		"relative CI root":         {"var/cache/ci", "/var/cache/release", true},
		"unclean CI root":          {"/var/cache/../cache/ci", "/var/cache/release", true},
		"trailing slash":           {"/var/cache/ci/", "/var/cache/release", true},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			config := testConfig()
			config.CICacheRoot = testCase.ci
			config.ReleaseCacheRoot = testCase.release
			err := config.Validate()
			if testCase.wantErr && err == nil {
				t.Fatalf("Validate accepted ci=%q release=%q", testCase.ci, testCase.release)
			}
			if !testCase.wantErr && err != nil {
				t.Fatalf("Validate refused a safe configuration: %v", err)
			}
		})
	}
}

// TestCacheReachesInlineTemplates proves the cache follows the same recursive
// walk as identity and environment, so a step nested in an inline template is
// not quietly left cold.
func TestCacheReachesInlineTemplates(t *testing.T) {
	const inlineDocument = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 600
  templates:
    - name: main
      dag:
        tasks:
          - name: nested
            inline:
              container:
                image: golang:1.26-trixie@sha256:ab563819a16cfe5faff0f96a8bb598fbb0e400ab2ac751996e60abcb23b106a3
                command: [/bin/true]
`
	workflow, err := Build(cachingConfig(), testRequest(periapsis.TriggerCI, inlineDocument))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	inline := workflow.Spec.Templates[0].DAG.Tasks[0].Inline
	if inline == nil || inline.Container == nil {
		t.Fatal("inline container disappeared")
	}
	if paths := mountPaths(inline.Container, CacheVolumeName); len(paths) != 1 {
		t.Fatalf("inline cache mount paths = %v, want exactly one", paths)
	}
	if value, ok := environmentValue(inline.Container, "GOCACHE"); !ok || value != BuildCachePath {
		t.Fatalf("inline GOCACHE = %q (present=%v), want %q", value, ok, BuildCachePath)
	}
}

// TestCacheRootForRefusesAnUnknownTrigger fails closed: a trigger the switch
// does not recognise gets no writable node directory at all.
func TestCacheRootForRefusesAnUnknownTrigger(t *testing.T) {
	if root := cachingConfig().cacheRootFor(periapsis.Trigger("promotion-of-some-kind")); root != "" {
		t.Fatalf("an unknown trigger resolved to cache root %q", root)
	}
}
