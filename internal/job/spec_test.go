package job

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

const testImage = "registry.invalid/oberth-ci@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func baseConfig() Config {
	return Config{
		Namespace:        "oberth",
		Image:            testImage,
		PVCName:          "oberth-data",
		CICacheRoot:      "/var/cache/oberth/ci",
		ReleaseCacheRoot: "/var/cache/oberth/release",
		ReleaseSecrets:   []string{"gar-sa-key", "r2-upload-token", "cosign-secret"},
	}
}

func baseRequest() Request {
	return Request{RunID: "r-0142", Repo: "cloudtaser-cli", Ref: "feature/test", SHA: strings.Repeat("a", 40)}
}

func testSecretSnapshot(request Request, secrets []ReleaseSecret) *SecretSnapshot {
	snapshot := &SecretSnapshot{
		Name: releaseSnapshotName(jobName(request)), Mounts: ReleaseSecretMounts{Secrets: secrets}, Data: make(map[string][]byte),
	}
	for secretIndex, secret := range secrets {
		for keyIndex := range secret.Keys {
			snapshot.Data[secretSnapshotDataKey(secretIndex, keyIndex)] = []byte("test-secret")
		}
	}
	snapshot.Digest, _ = secretSnapshotDigest(*snapshot)
	return snapshot
}

func TestCIBuildHasRequiredIsolationAndNoSecrets(t *testing.T) {
	request := baseRequest()
	job, err := Build(baseConfig(), request)
	if err != nil {
		t.Fatal(err)
	}
	if job.Namespace != "oberth" || job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Fatalf("job metadata/backoff = %#v", job)
	}
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 3600 {
		t.Fatalf("ttl = %v", job.Spec.TTLSecondsAfterFinished)
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != int64((12*time.Hour)/time.Second) {
		t.Fatalf("active deadline = %v", job.Spec.ActiveDeadlineSeconds)
	}
	if job.Labels["oberth.ci/trigger"] != "ci" || job.Spec.Template.Labels["oberth.ci/trigger"] != "ci" ||
		job.Spec.Template.Annotations["oberth.ci/ref"] != request.Ref {
		t.Fatalf("operational metadata = labels %#v, pod labels %#v, pod annotations %#v", job.Labels, job.Spec.Template.Labels, job.Spec.Template.Annotations)
	}
	pod := job.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatal("service account token is mounted")
	}
	if pod.RestartPolicy != corev1.RestartPolicyNever || pod.SecurityContext == nil || *pod.SecurityContext.RunAsUser != 65534 || *pod.SecurityContext.RunAsGroup != 65534 {
		t.Fatalf("pod security = %#v", pod.SecurityContext)
	}
	container := pod.Containers[0]
	if container.Image != testImage || container.SecurityContext == nil || container.SecurityContext.ReadOnlyRootFilesystem == nil || !*container.SecurityContext.ReadOnlyRootFilesystem {
		t.Fatalf("container security = %#v", container.SecurityContext)
	}
	repoDigest := sha256.Sum256([]byte(request.Repo))
	repoCache := request.Repo + "-" + hex.EncodeToString(repoDigest[:8])
	wantCaches := map[string]string{
		"gomod":   "/var/cache/oberth/ci/repos/" + repoCache + "/feature/gomod",
		"gobuild": "/var/cache/oberth/ci/repos/" + repoCache + "/feature/gobuild",
	}
	for _, volume := range pod.Volumes {
		if volume.Secret != nil {
			t.Fatalf("CI job contains secret volume %q", volume.Name)
		}
		if volume.HostPath != nil {
			if strings.Contains(volume.HostPath.Path, "/release/") {
				t.Fatalf("CI job uses release cache %q", volume.HostPath.Path)
			}
			if volume.HostPath.Type == nil || *volume.HostPath.Type != corev1.HostPathDirectory {
				t.Fatalf("CI cache %q type = %v, want Directory", volume.HostPath.Path, volume.HostPath.Type)
			}
			want, ok := wantCaches[volume.Name]
			if !ok || volume.HostPath.Path != want {
				t.Fatalf("unexpected CI cache volume %q at %q", volume.Name, volume.HostPath.Path)
			}
			delete(wantCaches, volume.Name)
		}
	}
	if len(wantCaches) != 0 {
		t.Fatalf("missing CI cache volumes: %v", wantCaches)
	}
	environment := make(map[string]string, len(container.Env))
	for _, variable := range container.Env {
		environment[variable.Name] = variable.Value
	}
	if environment["PATH"] != "/tmp/oberth-tools/bin:/usr/local/bin:/usr/bin:/bin" ||
		environment["OBERTH_TOOLS_DIR"] != "/tmp/oberth-tools" || environment["GOWORK"] != "off" ||
		environment["GIT_TERMINAL_PROMPT"] != "0" {
		t.Fatalf("tool bootstrap environment = %#v", environment)
	}
	if _, exists := environment["GOLANGCI_LINT_CACHE"]; exists {
		t.Fatalf("Job retains baked linter cache environment: %#v", environment)
	}
	if container.Resources.Requests.Cpu().IsZero() || container.Resources.Limits.Memory().IsZero() ||
		container.Resources.Requests.StorageEphemeral().IsZero() || container.Resources.Limits.StorageEphemeral().IsZero() {
		t.Fatal("resource requests/limits are missing")
	}
	foundBoundedTmp := false
	for _, volume := range pod.Volumes {
		if volume.Name == "tmp" && volume.EmptyDir != nil && volume.EmptyDir.SizeLimit != nil &&
			volume.EmptyDir.SizeLimit.Cmp(*container.Resources.Limits.StorageEphemeral()) == 0 {
			foundBoundedTmp = true
		}
	}
	if !foundBoundedTmp {
		t.Fatal("tmp emptyDir is not bounded by the Job ephemeral-storage limit")
	}
}

func TestReleaseBuildUsesOnlyReleaseCacheAndImmutableSnapshot(t *testing.T) {
	request := baseRequest()
	request.Release = true
	request.Ref = "refs/tags/v1.2.3"
	request.ReleaseSecrets = testSecretSnapshot(request, []ReleaseSecret{
		{Name: "gar-sa-key", Keys: []string{"service-account.json"}},
		{Name: "r2-upload-token", Keys: []string{"account", "token"}},
		{Name: "cosign-secret", Keys: []string{"password"}},
	})
	job, err := Build(baseConfig(), request)
	if err != nil {
		t.Fatal(err)
	}
	if job.Labels["oberth.ci/trigger"] != "release" || job.Spec.Template.Labels["oberth.ci/trigger"] != "release" ||
		job.Spec.Template.Annotations["oberth.ci/ref"] != request.Ref {
		t.Fatalf("release operational metadata = labels %#v, pod labels %#v, pod annotations %#v", job.Labels, job.Spec.Template.Labels, job.Spec.Template.Annotations)
	}
	secretCount := 0
	projectedKeys := 0
	repoDigest := sha256.Sum256([]byte(request.Repo))
	repoCache := request.Repo + "-" + hex.EncodeToString(repoDigest[:8])
	wantCaches := map[string]string{
		"gomod":   "/var/cache/oberth/release/repos/" + repoCache + "/release/gomod",
		"gobuild": "/var/cache/oberth/release/repos/" + repoCache + "/release/gobuild",
	}
	wantPaths := map[string]bool{
		"gar-sa-key/service-account.json": true,
		"r2-upload-token/account":         true,
		"r2-upload-token/token":           true,
		"cosign-secret/password":          true,
	}
	for _, volume := range job.Spec.Template.Spec.Volumes {
		if volume.HostPath != nil {
			if strings.Contains(volume.HostPath.Path, "/ci/") {
				t.Fatalf("release job uses CI cache %q", volume.HostPath.Path)
			}
			want, ok := wantCaches[volume.Name]
			if !ok || volume.HostPath.Path != want {
				t.Fatalf("unexpected release cache volume %q at %q", volume.Name, volume.HostPath.Path)
			}
			if volume.HostPath.Type == nil || *volume.HostPath.Type != corev1.HostPathDirectory {
				t.Fatalf("release cache %q type = %v, want Directory", volume.HostPath.Path, volume.HostPath.Type)
			}
			delete(wantCaches, volume.Name)
		}
		if volume.Secret != nil {
			secretCount++
			if volume.Secret.DefaultMode == nil || *volume.Secret.DefaultMode != 0o400 {
				t.Fatalf("release Secret projection mode = %v, want 0400", volume.Secret.DefaultMode)
			}
			if volume.Secret.SecretName != request.ReleaseSecrets.Name {
				t.Fatalf("release Job mounted source Secret %q instead of snapshot %q", volume.Secret.SecretName, request.ReleaseSecrets.Name)
			}
			if len(volume.Secret.Items) == 0 {
				t.Fatalf("secret volume %q projects every key", volume.Name)
			}
			for _, item := range volume.Secret.Items {
				if !wantPaths[item.Path] {
					t.Fatalf("secret item %q has unexpected projected path %q", item.Key, item.Path)
				}
				delete(wantPaths, item.Path)
			}
			projectedKeys += len(volume.Secret.Items)
		}
	}
	if len(wantCaches) != 0 {
		t.Fatalf("missing release cache volumes: %v", wantCaches)
	}
	if secretCount != 1 {
		t.Fatalf("secret volumes = %d, want 1 immutable snapshot", secretCount)
	}
	if projectedKeys != 4 {
		t.Fatalf("projected secret keys = %d, want 4", projectedKeys)
	}
	if len(wantPaths) != 0 {
		t.Fatalf("missing projected secret paths: %v", wantPaths)
	}
	podSecurity := job.Spec.Template.Spec.SecurityContext
	if podSecurity == nil || podSecurity.FSGroup == nil || *podSecurity.FSGroup != 65534 ||
		podSecurity.FSGroupChangePolicy == nil || *podSecurity.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch ||
		podSecurity.SupplementalGroupsPolicy == nil || *podSecurity.SupplementalGroupsPolicy != corev1.SupplementalGroupsPolicyStrict {
		t.Fatalf("release Secret reader group = %#v, want fsGroup 65534, OnRootMismatch, and Strict", podSecurity)
	}
	secretMounts := 0
	for _, mount := range job.Spec.Template.Spec.Containers[0].VolumeMounts {
		if mount.Name != releaseSnapshotVolumeName {
			continue
		}
		secretMounts++
		if !mount.ReadOnly || mount.MountPath != "/secrets" || mount.SubPath != "" {
			t.Fatalf("snapshot is not mounted read-only at /secrets: %#v", mount)
		}
	}
	if secretMounts != 1 {
		t.Fatalf("snapshot volume mounts = %d, want 1", secretMounts)
	}
}

func TestBuildIsolatesCachesByRepository(t *testing.T) {
	first, err := Build(baseConfig(), baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	secondRequest := baseRequest()
	secondRequest.RunID = "r-0143"
	secondRequest.Repo = "cloudtaser-beacon"
	second, err := Build(baseConfig(), secondRequest)
	if err != nil {
		t.Fatal(err)
	}
	paths := func(definition *batchv1.Job) map[string]string {
		result := make(map[string]string)
		for _, volume := range definition.Spec.Template.Spec.Volumes {
			if volume.HostPath != nil {
				result[volume.Name] = volume.HostPath.Path
			}
		}
		return result
	}
	firstPaths, secondPaths := paths(first), paths(second)
	for _, cache := range []string{"gomod", "gobuild"} {
		if firstPaths[cache] == secondPaths[cache] {
			t.Fatalf("repositories share %s cache %q", cache, firstPaths[cache])
		}
	}
}

func TestBuildIsolatesUntrustedFeatureCacheFromPromotion(t *testing.T) {
	feature, err := Build(baseConfig(), baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	promotionRequest := baseRequest()
	promotionRequest.RunID = "r-0143"
	promotionRequest.Trusted = true
	promotion, err := Build(baseConfig(), promotionRequest)
	if err != nil {
		t.Fatal(err)
	}
	paths := func(definition *batchv1.Job) map[string]string {
		result := make(map[string]string)
		for _, volume := range definition.Spec.Template.Spec.Volumes {
			if volume.HostPath != nil {
				result[volume.Name] = volume.HostPath.Path
			}
		}
		return result
	}
	featurePaths, promotionPaths := paths(feature), paths(promotion)
	for _, cache := range []string{"gomod", "gobuild"} {
		if featurePaths[cache] == promotionPaths[cache] || !strings.Contains(featurePaths[cache], "/feature/") ||
			!strings.Contains(promotionPaths[cache], "/promotion/") {
			t.Fatalf("%s trust-tier paths feature=%q promotion=%q", cache, featurePaths[cache], promotionPaths[cache])
		}
	}
}

func TestBuildRejectsInvalidOrDuplicateReleaseSecretNames(t *testing.T) {
	tests := []struct {
		name    string
		secrets []string
	}{
		{name: "uppercase", secrets: []string{"Gar-Key"}},
		{name: "underscore", secrets: []string{"gar_key"}},
		{name: "empty label", secrets: []string{"gar..key"}},
		{name: "trailing dash", secrets: []string{"gar-key-"}},
		{name: "too long", secrets: []string{strings.Repeat("a", 254)}},
		{name: "duplicate", secrets: []string{"gar-key", "gar-key"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := baseConfig()
			config.ReleaseSecrets = test.secrets
			if _, err := Build(config, baseRequest()); err == nil || !strings.Contains(err.Error(), "release Secret") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestBuildRejectsUnsafeOrMismatchedReleaseSecretKeys(t *testing.T) {
	valid := func() Request {
		request := baseRequest()
		request.Release = true
		request.ReleaseSecrets = testSecretSnapshot(request, []ReleaseSecret{
			{Name: "gar-sa-key", Keys: []string{"json"}},
			{Name: "r2-upload-token", Keys: []string{"token"}},
			{Name: "cosign-secret", Keys: []string{"password"}},
		})
		return request
	}
	tests := []struct {
		name      string
		configure func(*Request)
	}{
		{name: "missing snapshot", configure: func(request *Request) {
			request.ReleaseSecrets.Mounts.Secrets = request.ReleaseSecrets.Mounts.Secrets[:2]
		}},
		{name: "wrong configured name", configure: func(request *Request) { request.ReleaseSecrets.Mounts.Secrets[0].Name = "other-secret" }},
		{name: "path separator", configure: func(request *Request) { request.ReleaseSecrets.Mounts.Secrets[0].Keys[0] = "dir/token" }},
		{name: "parent traversal", configure: func(request *Request) { request.ReleaseSecrets.Mounts.Secrets[0].Keys[0] = ".." }},
		{name: "duplicate key", configure: func(request *Request) { request.ReleaseSecrets.Mounts.Secrets[0].Keys = []string{"json", "json"} }},
		{name: "CI carries secret snapshot", configure: func(request *Request) { request.Release = false }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := valid()
			test.configure(&request)
			if request.ReleaseSecrets != nil {
				digest, err := secretSnapshotDigest(*request.ReleaseSecrets)
				if err != nil {
					t.Fatal(err)
				}
				request.ReleaseSecrets.Digest = digest
			}
			if _, err := Build(baseConfig(), request); err == nil || !strings.Contains(strings.ToLower(err.Error()), "secret") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestBuildRejectsFloatingImageAndSharedCache(t *testing.T) {
	config := baseConfig()
	config.Image = "registry.invalid/oberth-ci:latest"
	config.ReleaseCacheRoot = config.CICacheRoot
	_, err := Build(config, baseRequest())
	if err == nil || !strings.Contains(err.Error(), "pinned") || !strings.Contains(err.Error(), "distinct") {
		t.Fatalf("error = %v", err)
	}
}

func TestBuildRejectsMalformedOrInvertedResourcesWithoutPanicking(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*Config)
	}{
		{name: "malformed CPU request", configure: func(config *Config) { config.CPURequest = "not-a-quantity" }},
		{name: "malformed memory request", configure: func(config *Config) { config.MemoryRequest = "2Gi!" }},
		{name: "zero CPU limit", configure: func(config *Config) { config.CPULimit = "0" }},
		{name: "negative memory limit", configure: func(config *Config) { config.MemoryLimit = "-1Gi" }},
		{name: "malformed ephemeral-storage request", configure: func(config *Config) { config.EphemeralStorageRequest = "many" }},
		{name: "CPU request above limit", configure: func(config *Config) { config.CPURequest, config.CPULimit = "4", "3" }},
		{name: "memory request above limit", configure: func(config *Config) { config.MemoryRequest, config.MemoryLimit = "5Gi", "4Gi" }},
		{name: "ephemeral-storage request above limit", configure: func(config *Config) {
			config.EphemeralStorageRequest, config.EphemeralStorageLimit = "9Gi", "8Gi"
		}},
		{name: "malformed step timeout", configure: func(config *Config) { config.StepTimeout = "eventually" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := baseConfig()
			test.configure(&config)
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf("Build panicked: %v", recovered)
				}
			}()
			if _, err := Build(config, baseRequest()); err == nil {
				t.Fatal("Build succeeded")
			}
		})
	}
}

func TestBuildRejectsNestedOrAliasedCacheRoots(t *testing.T) {
	tests := []struct {
		name    string
		ci      string
		release string
	}{
		{name: "release nested below CI", ci: "/var/cache/oberth/ci", release: "/var/cache/oberth/ci/gomod/release"},
		{name: "CI nested below release", ci: "/var/cache/oberth/release/gobuild/ci", release: "/var/cache/oberth/release"},
		{name: "same normalized path", ci: "/var/cache/oberth/ci", release: "/var/cache/oberth/ci"},
		{name: "derived alias", ci: "/var/cache/oberth/ci", release: "/var/cache/oberth/ci/../release"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := baseConfig()
			config.CICacheRoot, config.ReleaseCacheRoot = test.ci, test.release
			if _, err := Build(config, baseRequest()); err == nil || !strings.Contains(err.Error(), "cache roots") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestBuildUsesConfiguredOuterDeadline(t *testing.T) {
	config := baseConfig()
	config.JobTimeout = 90*time.Second + time.Nanosecond
	job, err := Build(config, baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 91 {
		t.Fatalf("active deadline = %v, want 91", job.Spec.ActiveDeadlineSeconds)
	}
}

func TestJobNamesRetainDistinctRunSuffixForLongRepositories(t *testing.T) {
	request := baseRequest()
	request.Repo = strings.Repeat("r", 128)
	request.RunID = strings.Repeat("a", 32)
	firstRunID := request.RunID
	first, err := Build(baseConfig(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.RunID = strings.Repeat("b", 32)
	second, err := Build(baseConfig(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Name) > 63 || len(second.Name) > 63 || first.Name == second.Name {
		t.Fatalf("job names = %q and %q", first.Name, second.Name)
	}
	for name, runID := range map[string]string{first.Name: firstRunID, second.Name: request.RunID} {
		digest := sha256.Sum256([]byte(runID))
		if suffix := hex.EncodeToString(digest[:jobNameHashBytes]); !strings.HasSuffix(name, "-"+suffix) {
			t.Fatalf("job name %q does not retain run suffix %q", name, suffix)
		}
	}
}

func TestBuildUsesValidatedExplicitJobNameAndSpecIdentity(t *testing.T) {
	request := baseRequest()
	request.JobName = "oberth-" + request.RunID
	first, err := Build(baseConfig(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Name != request.JobName {
		t.Fatalf("job name = %q, want %q", first.Name, request.JobName)
	}
	identity := first.Annotations["oberth.ci/spec-identity"]
	if len(identity) != sha256.Size*2 {
		t.Fatalf("spec identity = %q, want a SHA-256 hex digest", identity)
	}

	second, err := Build(baseConfig(), request)
	if err != nil {
		t.Fatal(err)
	}
	if second.Annotations["oberth.ci/spec-identity"] != identity {
		t.Fatalf("same request identities = %q and %q", identity, second.Annotations["oberth.ci/spec-identity"])
	}

	request.SHA = strings.Repeat("b", 40)
	changed, err := Build(baseConfig(), request)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Annotations["oberth.ci/spec-identity"] == identity {
		t.Fatal("spec identity did not change with the intended Job")
	}
}

func TestBuildRejectsInvalidExplicitJobName(t *testing.T) {
	for _, name := range []string{
		"Oberth-r-0142",
		"oberth_r-0142",
		"oberth-" + strings.Repeat("a", 57),
	} {
		t.Run(name, func(t *testing.T) {
			request := baseRequest()
			request.JobName = name
			if _, err := Build(baseConfig(), request); err == nil || !strings.Contains(err.Error(), "job name") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
