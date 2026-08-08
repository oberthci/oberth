package job

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/pkg/periapsis"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	k8scontent "k8s.io/apimachinery/pkg/api/validate/content"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
)

const (
	jobUID                          int64  = 65534
	defaultTTLSeconds               int32  = 3600
	defaultStepTimeout              string = "30m"
	defaultCPURequest                      = "1"
	defaultMemoryRequest                   = "2Gi"
	defaultCPULimit                        = "3"
	defaultMemoryLimit                     = "4Gi"
	defaultEphemeralStorageRequest         = "1Gi"
	defaultEphemeralStorageLimit           = "8Gi"
	defaultJobTimeout                      = 12 * time.Hour
	jobNameHashBytes                       = 12
	jobSpecIdentityAnnotation              = "oberth.ci/spec-identity"
	jobRunIDAnnotation                     = "oberth.ci/run-id"
	releaseSnapshotNameAnnotation          = "oberth.ci/release-secret"
	releaseSnapshotDigestAnnotation        = "oberth.ci/release-secret-digest"
	releaseSnapshotJobAnnotation           = "oberth.ci/release-job"
	releaseSnapshotNamePrefix              = "oberth-release-"
	releaseSnapshotVolumeName              = "release-secrets"
	runnerContainerName                    = "run"
	runnerBinaryPath                       = "/usr/local/bin/oberth-runner"
	secretStoreVolumeName                  = "secretstore"
	secretStoreMountPath                   = "/run/secrets/secretstore"
	secretStoreAnnotation                  = "oberth.ci/secretstore-secrets"
	secretStoreVolumeSizeLimit             = "32Mi"
)

var digestImagePattern = regexp.MustCompile(`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`)

type Config struct {
	Namespace          string
	Image              string
	PullPolicy         corev1.PullPolicy
	PVCName            string
	CICacheRoot        string
	ReleaseCacheRoot   string
	ProvisionCacheDirs bool
	ReleaseSecrets     []string
	// SecretStorePaths is the administrator allowlist of OpenBao KV paths a
	// repository may request through its literal SecretStoreSecrets declaration.
	SecretStorePaths        []string
	CPURequest              string
	MemoryRequest           string
	CPULimit                string
	MemoryLimit             string
	EphemeralStorageRequest string
	EphemeralStorageLimit   string
	StepTimeout             string
	JobTimeout              time.Duration
	TTLSeconds              int32
}

// ReleaseSecret records the original configured Secret name and sorted data
// keys whose immutable snapshot is mounted for one release Job.
type ReleaseSecret struct {
	Name string
	Keys []string
}

type ReleaseSecretMounts struct {
	Secrets []ReleaseSecret
}

type SecretSnapshot struct {
	Name   string
	Digest string
	Mounts ReleaseSecretMounts
	Data   map[string][]byte
}

// MaskValues returns immutable copies of every unique nonempty snapshotted
// value. The streaming redactor expands multiline values into line masks.
func (snapshot SecretSnapshot) MaskValues() [][]byte {
	keys := make([]string, 0, len(snapshot.Data))
	for key := range snapshot.Data {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	seen := make(map[string]bool, len(keys))
	values := make([][]byte, 0, len(keys))
	for _, key := range keys {
		value := snapshot.Data[key]
		if len(value) == 0 || seen[string(value)] {
			continue
		}
		values = append(values, slices.Clone(value))
		seen[string(value)] = true
	}
	return values
}

type Request struct {
	RunID          string
	JobName        string
	Repo           string
	Ref            string
	SHA            string
	Release        bool
	Trusted        bool
	ReleaseSecrets *SecretSnapshot
	// SecretStoreSecrets is annotation-safe metadata only. Fetched values stay in
	// server memory and reach the Pod exclusively through the exec delivery
	// channel — never through the Job spec, a Secret, or etcd.
	SecretStoreSecrets []SecretStoreSource
}

func Build(config Config, request Request) (*batchv1.Job, error) {
	applyDefaults(&config)
	resources, err := validate(config, request)
	if err != nil {
		return nil, err
	}

	zero := int32(0)
	activeDeadlineSeconds := durationSeconds(config.JobTimeout)
	nonRoot := true
	noPrivilegeEscalation := false
	readOnlyRoot := true
	uid, gid := jobUID, jobUID
	fileSystemGroupPolicy := corev1.FSGroupChangeOnRootMismatch
	supplementalGroupsPolicy := corev1.SupplementalGroupsPolicyStrict
	terminationGracePeriod := int64(10)
	tmpSizeLimit := resources.Limits[corev1.ResourceEphemeralStorage]
	trigger := "ci"
	annotations := map[string]string{jobRunIDAnnotation: request.RunID}
	if request.Release {
		trigger = "release"
		annotations[releaseSnapshotNameAnnotation] = request.ReleaseSecrets.Name
		annotations[releaseSnapshotDigestAnnotation] = request.ReleaseSecrets.Digest
		if len(request.SecretStoreSecrets) != 0 {
			// Names and paths mirror the repository's public declaration; values
			// and value-derived digests must never appear in a Kubernetes object.
			encodedSources, err := json.Marshal(request.SecretStoreSecrets)
			if err != nil {
				return nil, fmt.Errorf("encode secret store secret sources: %w", err)
			}
			annotations[secretStoreAnnotation] = string(encodedSources)
		}
	}
	repositoryCacheRoot := filepath.Join(requestCacheRoot(config, request), repositoryCacheRelativePath(request))
	labels := map[string]string{
		"app.kubernetes.io/name": "oberth-run",
		"oberth.ci/repo":         labelValue(request.Repo),
		"oberth.ci/run":          labelValue(request.RunID),
		"oberth.ci/sha":          labelValue(request.SHA),
		"oberth.ci/trigger":      trigger,
	}
	podAnnotations := map[string]string{"oberth.ci/ref": request.Ref, jobRunIDAnnotation: request.RunID}
	podSecurityContext := &corev1.PodSecurityContext{
		RunAsNonRoot: &nonRoot,
		RunAsUser:    &uid,
		RunAsGroup:   &gid,
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
	if request.Release {
		// SecretVolumeSource files are created as root. The fsGroup makes the
		// 0400 projection readable as 0440 by the single UID/GID 65534 runner
		// without making release credentials world-readable or writable.
		podSecurityContext.FSGroup = &gid
		podSecurityContext.FSGroupChangePolicy = &fileSystemGroupPolicy
		podSecurityContext.SupplementalGroupsPolicy = &supplementalGroupsPolicy
	}

	volumes := []corev1.Volume{
		{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: config.PVCName}}},
		{Name: "gomod", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: filepath.Join(repositoryCacheRoot, "gomod"), Type: hostPathType(corev1.HostPathDirectory)}}},
		{Name: "gobuild", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: filepath.Join(repositoryCacheRoot, "gobuild"), Type: hostPathType(corev1.HostPathDirectory)}}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &tmpSizeLimit}}},
	}
	mounts := []corev1.VolumeMount{
		{Name: "data", MountPath: "/work", SubPath: "work/" + request.RunID},
		{Name: "gomod", MountPath: "/cache/gomod"},
		{Name: "gobuild", MountPath: "/cache/gobuild"},
		{Name: "tmp", MountPath: "/tmp"},
	}
	if request.Release {
		items := make([]corev1.KeyToPath, 0, len(request.ReleaseSecrets.Data))
		for secretIndex, secret := range request.ReleaseSecrets.Mounts.Secrets {
			keys := slices.Clone(secret.Keys)
			slices.Sort(keys)
			for keyIndex, key := range keys {
				projectionPath := secretSnapshotDataKey(secretIndex, keyIndex)
				items = append(items, corev1.KeyToPath{Key: projectionPath, Path: secret.Name + "/" + key})
			}
		}
		if len(items) != 0 {
			volumes = append(volumes, corev1.Volume{Name: releaseSnapshotVolumeName, VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: request.ReleaseSecrets.Name, Items: items, DefaultMode: int32Pointer(0o400)},
			}})
			mounts = append(mounts, corev1.VolumeMount{
				Name: releaseSnapshotVolumeName, MountPath: "/secrets", ReadOnly: true,
			})
		}
		if len(request.SecretStoreSecrets) != 0 {
			// medium: Memory keeps exec-delivered secret store values on tmpfs — RAM
			// only, never etcd, never node disk. The mount stays writable for
			// the delivery helper; runner, helper, and steps share UID 65534.
			storeSizeLimit := resource.MustParse(secretStoreVolumeSizeLimit)
			volumes = append(volumes, corev1.Volume{Name: secretStoreVolumeName, VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: &storeSizeLimit},
			}})
			mounts = append(mounts, corev1.VolumeMount{Name: secretStoreVolumeName, MountPath: secretStoreMountPath})
		}
	}

	runnerArgs := []string{
		"--src", "/work/src",
		"--periapsis", ".oberth/periapsis.go",
		"--trigger", trigger,
		"--step-timeout", config.StepTimeout,
		"--termination-log", "/dev/termination-log",
		"--secret-dir", "/secrets",
	}
	runnerEnv := []corev1.EnvVar{
		{Name: "PATH", Value: "/tmp/oberth-tools/bin:/usr/local/bin:/usr/bin:/bin"},
		{Name: "OBERTH_TOOLS_DIR", Value: "/tmp/oberth-tools"},
		{Name: "GOWORK", Value: "off"},
		{Name: "GIT_TERMINAL_PROMPT", Value: "0"},
		{Name: "GOMODCACHE", Value: "/cache/gomod"},
		{Name: "GOCACHE", Value: "/cache/gobuild"},
		{Name: "OBERTH_REPO", Value: request.Repo},
		{Name: "OBERTH_REF", Value: request.Ref},
		{Name: "OBERTH_SHA", Value: request.SHA},
		{Name: "OBERTH_TRIGGER", Value: trigger},
	}
	if request.Release && len(request.SecretStoreSecrets) != 0 {
		runnerArgs = append(runnerArgs, "--secretstore-dir", secretStoreMountPath)
		runnerEnv = append(runnerEnv, corev1.EnvVar{Name: "OBERTH_SECRETSTORE_DIR", Value: secretStoreMountPath})
	}

	definition := &batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        jobName(request),
			Namespace:   config.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &zero,
			ActiveDeadlineSeconds:   &activeDeadlineSeconds,
			TTLSecondsAfterFinished: &config.TTLSeconds,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: podAnnotations},
				Spec: corev1.PodSpec{
					RestartPolicy:                 corev1.RestartPolicyNever,
					AutomountServiceAccountToken:  boolPointer(false),
					TerminationGracePeriodSeconds: &terminationGracePeriod,
					SecurityContext:               podSecurityContext,
					Containers: []corev1.Container{{
						Name:            runnerContainerName,
						Image:           config.Image,
						ImagePullPolicy: config.PullPolicy,
						Command:         []string{runnerBinaryPath},
						Args:            runnerArgs,
						WorkingDir:      "/work/src",
						Env:             runnerEnv,
						Resources:       resources,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &noPrivilegeEscalation,
							ReadOnlyRootFilesystem:   &readOnlyRoot,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						VolumeMounts: mounts,
					}},
					Volumes: volumes,
				},
			},
		},
	}
	identity, err := jobSpecIdentity(definition)
	if err != nil {
		return nil, err
	}
	definition.Annotations[jobSpecIdentityAnnotation] = identity
	return definition, nil
}

func cacheTrustTier(request Request) string {
	if request.Release {
		return "release"
	}
	if request.Trusted {
		return "promotion"
	}
	return "feature"
}

func requestCacheRoot(config Config, request Request) string {
	if request.Release {
		return config.ReleaseCacheRoot
	}
	return config.CICacheRoot
}

func repositoryCacheRelativePath(request Request) string {
	return filepath.Join("repos", repositoryCacheKey(request.Repo), cacheTrustTier(request))
}

func repositoryCacheKey(repository string) string {
	digest := sha256.Sum256([]byte(repository))
	return repository + "-" + hex.EncodeToString(digest[:8])
}

func validate(config Config, request Request) (corev1.ResourceRequirements, error) {
	var problems []error
	if strings.TrimSpace(config.Namespace) == "" {
		problems = append(problems, errors.New("namespace is required"))
	}
	if !digestImagePattern.MatchString(config.Image) {
		problems = append(problems, errors.New("runner image must be pinned by sha256 digest"))
	}
	if strings.TrimSpace(config.PVCName) == "" {
		problems = append(problems, errors.New("PVC name is required"))
	}
	if err := validateCacheRoots(config.CICacheRoot, config.ReleaseCacheRoot); err != nil {
		problems = append(problems, err)
	}
	if err := validateReleaseSecretNames(config.ReleaseSecrets); err != nil {
		problems = append(problems, err)
	}
	if !safeName(request.RunID) {
		problems = append(problems, errors.New("safe run ID is required"))
	}
	if request.JobName != "" && (len(request.JobName) > 63 || len(k8svalidation.IsDNS1123Subdomain(request.JobName)) != 0) {
		problems = append(problems, errors.New("job name must be a DNS-1123 subdomain no longer than 63 characters"))
	}
	if normalized, err := gitcache.NormalizeRepo(request.Repo); err != nil || normalized != request.Repo {
		problems = append(problems, errors.New("canonical repository name is required"))
	}
	if !fullSHA(request.SHA) {
		problems = append(problems, errors.New("SHA must be a lowercase 40-character object ID"))
	}
	if strings.TrimSpace(request.Ref) == "" || strings.ContainsAny(request.Ref, "\x00\r\n") {
		problems = append(problems, errors.New("ref is required and must not contain control characters"))
	}
	if err := validateRequestSecrets(config.ReleaseSecrets, request); err != nil {
		problems = append(problems, err)
	}
	if err := validateSecretStorePathAllowlist(config.SecretStorePaths); err != nil {
		problems = append(problems, err)
	}
	if err := validateRequestSecretStoreSecrets(config, request); err != nil {
		problems = append(problems, err)
	}
	if timeout, err := time.ParseDuration(config.StepTimeout); err != nil || timeout <= 0 {
		problems = append(problems, fmt.Errorf("step timeout %q must be a positive duration", config.StepTimeout))
	}
	if config.JobTimeout <= 0 {
		problems = append(problems, errors.New("job timeout must be greater than zero"))
	}
	resources, err := parseResourceRequirements(config)
	if err != nil {
		problems = append(problems, err)
	}
	return resources, errors.Join(problems...)
}

func validateReleaseSecretNames(names []string) error {
	var problems []error
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if messages := k8svalidation.IsDNS1123Subdomain(name); len(messages) != 0 {
			problems = append(problems, fmt.Errorf("release Secret name %q must be a DNS-1123 subdomain: %s", name, strings.Join(messages, ", ")))
		}
		if seen[name] {
			problems = append(problems, fmt.Errorf("release Secret name %q is duplicated", name))
		}
		seen[name] = true
	}
	return errors.Join(problems...)
}

func validateRequestSecrets(configured []string, request Request) error {
	if !request.Release {
		if request.ReleaseSecrets != nil {
			return errors.New("CI request must not include release Secret keys")
		}
		return nil
	}
	if request.ReleaseSecrets == nil {
		return errors.New("release Secret snapshot is required")
	}
	expectedName := releaseSnapshotName(jobName(request))
	if request.ReleaseSecrets.Name != expectedName {
		return fmt.Errorf("release Secret snapshot name is %q, want %q", request.ReleaseSecrets.Name, expectedName)
	}
	var problems []error
	allowed := make(map[string]struct{}, len(configured))
	for _, name := range configured {
		allowed[name] = struct{}{}
	}
	if len(request.ReleaseSecrets.Mounts.Secrets) > len(allowed) {
		problems = append(problems, fmt.Errorf("release Secret snapshot has more entries than the administrator allowlist"))
	}
	expectedDataKeys := make(map[string]bool, len(request.ReleaseSecrets.Data))
	seenNames := make(map[string]struct{}, len(request.ReleaseSecrets.Mounts.Secrets))
	for index, secret := range request.ReleaseSecrets.Mounts.Secrets {
		if _, ok := allowed[secret.Name]; !ok {
			problems = append(problems, fmt.Errorf("release Secret snapshot entry %d names non-allowed Secret %q", index, secret.Name))
		}
		if _, duplicate := seenNames[secret.Name]; duplicate {
			problems = append(problems, fmt.Errorf("release Secret snapshot name %q is duplicated", secret.Name))
		}
		seenNames[secret.Name] = struct{}{}
		if !slices.IsSorted(secret.Keys) {
			problems = append(problems, fmt.Errorf("release Secret %q data keys are not sorted", secret.Name))
		}
		seen := make(map[string]bool, len(secret.Keys))
		for keyIndex, key := range secret.Keys {
			if err := validateReleaseSecretKey(secret.Name, key); err != nil {
				problems = append(problems, err)
			}
			if seen[key] {
				problems = append(problems, fmt.Errorf("release Secret %q data key %q is duplicated", secret.Name, key))
			}
			seen[key] = true
			dataKey := secretSnapshotDataKey(index, keyIndex)
			expectedDataKeys[dataKey] = true
			if _, exists := request.ReleaseSecrets.Data[dataKey]; !exists {
				problems = append(problems, fmt.Errorf("release Secret snapshot data key %q is missing", dataKey))
			}
		}
	}
	for key := range request.ReleaseSecrets.Data {
		if !expectedDataKeys[key] {
			problems = append(problems, fmt.Errorf("release Secret snapshot has unexpected data key %q", key))
		}
	}
	digest, err := secretSnapshotDigest(*request.ReleaseSecrets)
	if err != nil {
		problems = append(problems, err)
	} else if request.ReleaseSecrets.Digest != digest {
		problems = append(problems, errors.New("release Secret snapshot digest does not match its exact data"))
	}
	return errors.Join(problems...)
}

func validateSecretStorePathAllowlist(paths []string) error {
	var problems []error
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		if err := periapsis.ValidateSecretStorePath(path); err != nil {
			problems = append(problems, err)
		}
		if seen[path] {
			problems = append(problems, fmt.Errorf("secret store secret path %q is duplicated in the allowlist", path))
		}
		seen[path] = true
	}
	return errors.Join(problems...)
}

// validateRequestSecretStoreSecrets is the last admission gate before the Job object
// exists: the trigger split (branch burns get zero secret store secrets), the
// administrator path allowlist, and the delivery-name grammar are re-proven on
// the exact request even though the controller checked them at snapshot time.
func validateRequestSecretStoreSecrets(config Config, request Request) error {
	if !request.Release {
		if len(request.SecretStoreSecrets) != 0 {
			return errors.New("CI request must not include secret store secret sources")
		}
		return nil
	}
	if len(request.SecretStoreSecrets) > maxSecretStoreSources {
		return fmt.Errorf("release request declares %d secret store secrets, maximum is %d", len(request.SecretStoreSecrets), maxSecretStoreSources)
	}
	allowed := make(map[string]struct{}, len(config.SecretStorePaths))
	for _, path := range config.SecretStorePaths {
		allowed[path] = struct{}{}
	}
	taken := make(map[string]struct{})
	if request.ReleaseSecrets != nil {
		for _, secret := range request.ReleaseSecrets.Mounts.Secrets {
			taken[secret.Name] = struct{}{}
		}
	}
	var problems []error
	seenPaths := make(map[string]struct{}, len(request.SecretStoreSecrets))
	for index, source := range request.SecretStoreSecrets {
		if messages := k8svalidation.IsDNS1123Subdomain(source.Name); len(messages) != 0 {
			problems = append(problems, fmt.Errorf("secret store name %q must be a DNS-1123 subdomain: %s", source.Name, strings.Join(messages, ", ")))
		}
		if index > 0 && strings.Compare(request.SecretStoreSecrets[index-1].Name, source.Name) >= 0 {
			problems = append(problems, fmt.Errorf("secret store secret sources must be strictly sorted by name; %q is out of order", source.Name))
		}
		if _, collision := taken[source.Name]; collision {
			problems = append(problems, fmt.Errorf("secret store name %q collides with a Kubernetes release Secret mount", source.Name))
		}
		taken[source.Name] = struct{}{}
		if err := periapsis.ValidateSecretStorePath(source.Path); err != nil {
			problems = append(problems, err)
		}
		if _, duplicate := seenPaths[source.Path]; duplicate {
			problems = append(problems, fmt.Errorf("secret store path %q is declared more than once", source.Path))
		}
		seenPaths[source.Path] = struct{}{}
		if _, ok := allowed[source.Path]; !ok {
			problems = append(problems, fmt.Errorf("secret store path %q is not in the administrator allowlist", source.Path))
		}
	}
	return errors.Join(problems...)
}

func validateReleaseSecretKey(secretName, key string) error {
	if key == "." || key == ".." {
		return fmt.Errorf("release Secret %q data key %q is unsafe", secretName, key)
	}
	if messages := k8svalidation.IsConfigMapKey(key); len(messages) != 0 {
		return fmt.Errorf("release Secret %q data key %q is unsafe: %s", secretName, key, strings.Join(messages, ", "))
	}
	return nil
}

func applyDefaults(config *Config) {
	if config.PullPolicy == "" {
		config.PullPolicy = corev1.PullIfNotPresent
	}
	if config.CPURequest == "" {
		config.CPURequest = defaultCPURequest
	}
	if config.MemoryRequest == "" {
		config.MemoryRequest = defaultMemoryRequest
	}
	if config.CPULimit == "" {
		config.CPULimit = defaultCPULimit
	}
	if config.MemoryLimit == "" {
		config.MemoryLimit = defaultMemoryLimit
	}
	if config.EphemeralStorageRequest == "" {
		config.EphemeralStorageRequest = defaultEphemeralStorageRequest
	}
	if config.EphemeralStorageLimit == "" {
		config.EphemeralStorageLimit = defaultEphemeralStorageLimit
	}
	if config.StepTimeout == "" {
		config.StepTimeout = defaultStepTimeout
	}
	if config.JobTimeout == 0 {
		config.JobTimeout = defaultJobTimeout
	}
	if config.TTLSeconds <= 0 {
		config.TTLSeconds = defaultTTLSeconds
	}
}

func validateCacheRoots(ciRoot, releaseRoot string) error {
	ci, err := cleanCacheRoot(ciRoot)
	if err != nil {
		return fmt.Errorf("CI cache root: %w", err)
	}
	release, err := cleanCacheRoot(releaseRoot)
	if err != nil {
		return fmt.Errorf("release cache root: %w", err)
	}
	if pathContains(ci, release) || pathContains(release, ci) {
		return errors.New("CI and release cache roots must be distinct non-nested paths")
	}
	return nil
}

func cleanCacheRoot(value string) (string, error) {
	if value == "" || !filepath.IsAbs(value) || strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("cache roots must be absolute paths without control characters")
	}
	clean := filepath.Clean(value)
	if clean != value || clean == string(filepath.Separator) {
		return "", errors.New("cache roots must be canonical non-root paths")
	}
	return clean, nil
}

func pathContains(parent, candidate string) bool {
	relative, err := filepath.Rel(parent, candidate)
	return err == nil && filepath.IsLocal(relative)
}

func parseResourceRequirements(config Config) (corev1.ResourceRequirements, error) {
	cpuRequest, cpuRequestErr := parsePositiveQuantity("CPU request", config.CPURequest)
	memoryRequest, memoryRequestErr := parsePositiveQuantity("memory request", config.MemoryRequest)
	cpuLimit, cpuLimitErr := parsePositiveQuantity("CPU limit", config.CPULimit)
	memoryLimit, memoryLimitErr := parsePositiveQuantity("memory limit", config.MemoryLimit)
	ephemeralStorageRequest, ephemeralStorageRequestErr := parsePositiveQuantity("ephemeral-storage request", config.EphemeralStorageRequest)
	ephemeralStorageLimit, ephemeralStorageLimitErr := parsePositiveQuantity("ephemeral-storage limit", config.EphemeralStorageLimit)
	if err := errors.Join(cpuRequestErr, memoryRequestErr, cpuLimitErr, memoryLimitErr, ephemeralStorageRequestErr, ephemeralStorageLimitErr); err != nil {
		return corev1.ResourceRequirements{}, err
	}
	var problems []error
	if cpuRequest.Cmp(cpuLimit) > 0 {
		problems = append(problems, errors.New("CPU request must not exceed CPU limit"))
	}
	if memoryRequest.Cmp(memoryLimit) > 0 {
		problems = append(problems, errors.New("memory request must not exceed memory limit"))
	}
	if ephemeralStorageRequest.Cmp(ephemeralStorageLimit) > 0 {
		problems = append(problems, errors.New("ephemeral-storage request must not exceed ephemeral-storage limit"))
	}
	if err := errors.Join(problems...); err != nil {
		return corev1.ResourceRequirements{}, err
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: cpuRequest, corev1.ResourceMemory: memoryRequest,
			corev1.ResourceEphemeralStorage: ephemeralStorageRequest,
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU: cpuLimit, corev1.ResourceMemory: memoryLimit,
			corev1.ResourceEphemeralStorage: ephemeralStorageLimit,
		},
	}, nil
}

func parsePositiveQuantity(name, value string) (resource.Quantity, error) {
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("%s %q is invalid: %w", name, value, err)
	}
	if quantity.Sign() <= 0 {
		return resource.Quantity{}, fmt.Errorf("%s must be greater than zero", name)
	}
	return quantity, nil
}

func safeName(value string) bool {
	if value == "" || len(value) > 63 || strings.HasPrefix(value, ".") || strings.HasPrefix(value, "-") {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '.' || character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}

func fullSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func labelValue(value string) string {
	var result strings.Builder
	for _, character := range strings.ToLower(value) {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' {
			result.WriteRune(character)
		} else {
			result.WriteByte('-')
		}
	}
	trimmed := strings.Trim(result.String(), "-_.")
	if len(trimmed) > k8scontent.LabelValueMaxLength {
		trimmed = trimmed[:k8scontent.LabelValueMaxLength]
	}
	return strings.TrimRight(trimmed, "-_.")
}

func jobName(request Request) string {
	if request.JobName != "" {
		return request.JobName
	}
	prefix := "ci"
	if request.Release {
		prefix = "release"
	}
	digest := sha256.Sum256([]byte(request.RunID))
	suffix := hex.EncodeToString(digest[:jobNameHashBytes])
	repository := dnsNameFragment(request.Repo)
	maximumRepositoryBytes := 63 - len(prefix) - len(suffix) - 2
	if len(repository) > maximumRepositoryBytes {
		repository = strings.TrimRight(repository[:maximumRepositoryBytes], "-.")
	}
	return prefix + "-" + repository + "-" + suffix
}

func releaseSnapshotName(job string) string {
	digest := sha256.Sum256([]byte(job))
	return releaseSnapshotNamePrefix + hex.EncodeToString(digest[:24])
}

func secretSnapshotDataKey(secretIndex, keyIndex int) string {
	return fmt.Sprintf("secret-%d-key-%d", secretIndex, keyIndex)
}

func secretSnapshotDigest(snapshot SecretSnapshot) (string, error) {
	payload := struct {
		Name   string              `json:"name"`
		Mounts ReleaseSecretMounts `json:"mounts"`
		Data   map[string][]byte   `json:"data"`
	}{Name: snapshot.Name, Mounts: snapshot.Mounts, Data: snapshot.Data}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode release Secret snapshot identity: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func jobSpecIdentity(definition *batchv1.Job) (string, error) {
	encoded, err := json.Marshal(definition)
	if err != nil {
		return "", fmt.Errorf("encode intended Job identity: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func dnsNameFragment(value string) string {
	var result strings.Builder
	for _, character := range strings.ToLower(value) {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' || character == '.' {
			result.WriteRune(character)
		} else {
			result.WriteByte('-')
		}
	}
	return strings.Trim(result.String(), "-.")
}

func durationSeconds(value time.Duration) int64 {
	seconds := int64(value / time.Second)
	if value%time.Second != 0 {
		seconds++
	}
	return seconds
}

func hostPathType(value corev1.HostPathType) *corev1.HostPathType { return &value }
func boolPointer(value bool) *bool                                { return &value }
func int32Pointer(value int32) *int32                             { return &value }
