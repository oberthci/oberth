package argojob

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// A pipeline's containers must see the exact pushed revision, and the server
// already has it: it checked the revision out onto its own PersistentVolume
// before scheduling anything.
//
// It cannot simply hand that volume over. A PersistentVolumeClaim is
// namespace-scoped and a Pod may only reference claims in its own namespace,
// while pipelines deliberately run in a namespace of their own so that
// OpenBao's (namespace, ServiceAccount) role bindings separate pipeline
// identities from server ones. The claim the server mounts is therefore
// unreachable from the pods that need it — a constraint that stays invisible
// until the two namespaces are actually different, which is why it survived a
// canary whose source claim was seeded into the pipeline namespace by hand.
//
// Rather than weaken the namespace split, the server copies the revision into a
// claim of its own making in the pipeline namespace, over the Kubernetes exec
// stream — the same transport release secrets already travel on, chosen for the
// same reason: it needs no credential in the pipeline namespace, no network
// listener, and no shared storage backend. The alternative designs all end in a
// credential the CI tier is specifically supposed not to hold: a git clone from
// Oberth needs an uplink key, and an uplink key can promote to main.
//
// The claim is created per run, mounted read-only by every pipeline container,
// and owned by the Workflow so it is collected with it.

const (
	// seedContainer is the only container in the seeding Pod.
	seedContainer = "seed"

	// seedMountPath is where the seeding Pod mounts the run's claim. The
	// archive expands here, so the layout under it is what pipeline containers
	// later see at SourceMountPath.
	seedMountPath = "/seed"

	// sourceClaimLabelSelector matches the claims this seeder creates, which is
	// what makes an unowned one identifiable as ours rather than a coincidence.
	sourceClaimLabelSelector = "oberth.ci/component=source"

	// defaultOrphanClaimGrace is how long a claim may exist without an owner
	// before the sweep treats it as abandoned. It must comfortably exceed the
	// time between creating a claim and adopting its Workflow.
	defaultOrphanClaimGrace = time.Hour

	// maxSourceArchiveBytes bounds what the server will stream. A repository
	// checkout is source, not artifacts, and a runaway working tree should
	// fail one run rather than fill the pipeline namespace's storage.
	maxSourceArchiveBytes = 512 << 20

	// vaultCASubPath holds the OpenBao TLS trust anchor, as a sibling of the
	// checkout rather than a file inside it. A pipeline sees the repository's
	// own tree at SourceMountPath and nothing the server added to it, and a
	// repository cannot shadow the anchor with a file of its own.
	vaultCASubPath  = "vault-ca"
	vaultCACertFile = "ca.crt"

	// binarySubPath holds the server binary, delivered alongside the checkout
	// so pipeline containers can invoke `oberth secretstore exec` without
	// fetching the binary from a registry or baking it into an image.
	binarySubPath  = "oberth-bin"
	binaryFileName = "oberth"

	artifactsSubPath = "artifacts"
)

// SourceVolume describes the per-run claim a Workflow's containers mount.
type SourceVolume struct {
	// ClaimName is the PersistentVolumeClaim in the pipeline namespace.
	ClaimName string
	// SubPath is the directory inside the claim holding the checkout.
	SubPath string
	// VaultCASubPath is the directory inside the same claim holding the
	// OpenBao TLS trust anchor, and is empty unless this run's seeding
	// actually wrote it. Build reads it rather than the configured PEM so the
	// mount and the VAULT_CACERT it advertises cannot disagree: a path
	// injected for a file that was never written is a step that fails its
	// login on a missing file instead of a missing anchor.
	VaultCASubPath string

	ArtifactsSubPath string
	// BinarySubPath is the directory inside the claim holding the Oberth
	// server binary, and is empty unless this run's seeding actually wrote
	// it. Build mounts it read-only only for assisted continuation or credentialed
	// secretstore execution. Neither use fetches a binary
	// from a registry or requires baking it into a pipeline image.
	BinarySubPath string
}

// SourceSeeder populates a per-run source claim in the pipeline namespace.
type SourceSeeder struct {
	observationsMu sync.Mutex
	observations   map[string]sourceSeedIdentity
	hub            ObservationHub
	workflows      WorkflowClient
	client         kubernetes.Interface
	exec           ExecStreamer
	config         Config
}

// ExecStreamer runs a command in a container and streams stdin to it. It is the
// same shape the release-secret delivery uses, so a deployment wires one
// implementation for both.
type ExecStreamer func(ctx context.Context, namespace, pod, container string, command []string,
	stdin io.Reader, stdout, stderr io.Writer) error

// NewSourceSeeder returns a seeder, or nil when the deployment did not wire the
// Kubernetes access it needs. A nil seeder is not a silent degradation: Create
// refuses to submit a Workflow it cannot give a source volume to.
func NewSourceSeeder(client kubernetes.Interface, exec ExecStreamer, config Config) *SourceSeeder {
	if client == nil || exec == nil {
		return nil
	}
	// The seeder takes a Config by value the way Build does, so it must apply
	// the same defaults. Without this the size below is the zero value.
	config.applyDefaults()
	return &SourceSeeder{client: client, exec: exec, config: config}
}

// Seed creates the run's claim and fills it with the checkout at sourceDir,
// including a verified executable only for assisted continuation, and for credentialed
// runs the OpenBao TLS trust anchor beside it.
//
// On any failure the claim is removed, so a failed submission does not leave
// storage behind in the pipeline namespace.
func (seeder *SourceSeeder) Seed(
	ctx context.Context, workflowName, sourceDir string, deliverStoreAssets bool,
) (SourceVolume, error) {
	return seeder.seed(ctx, workflowName, sourceDir, deliverStoreAssets, false)
}

// SeedRequest carries the complete durable selection before any claim or Pod is created.
func (seeder *SourceSeeder) SeedRequest(ctx context.Context, request Request, deliverStoreAssets bool) (SourceVolume, error) {
	return seeder.seed(ctx, request.Name, request.SourceDir, deliverStoreAssets, request.Assist, &request)
}

func (seeder *SourceSeeder) seed(ctx context.Context, workflowName, sourceDir string, deliverStoreAssets, assistHelper bool, requests ...*Request) (SourceVolume, error) {
	volume := SourceVolume{ClaimName: sourceClaimName(workflowName), SubPath: "src"}
	if strings.TrimSpace(sourceDir) == "" {
		return SourceVolume{}, errors.New("argojob: a source checkout directory is required")
	}
	info, err := os.Stat(sourceDir)
	if err != nil {
		return SourceVolume{}, fmt.Errorf("argojob: read source checkout: %w", err)
	}
	if !info.IsDir() {
		return SourceVolume{}, fmt.Errorf("argojob: source checkout %s is not a directory", sourceDir)
	}

	// Collect anything a previous run left unowned before adding storage of our
	// own. Doing it here rather than on a timer means the sweep runs exactly
	// when the namespace is about to grow, and needs no scheduler of its own.
	seeder.SweepOrphanedSourceClaims(ctx)

	if err := seeder.createClaim(ctx, volume.ClaimName, workflowName); err != nil {
		return SourceVolume{}, err
	}
	if err := seeder.fill(ctx, &volume, sourceDir, deliverStoreAssets, assistHelper, requests...); err != nil {
		seeder.DeleteClaim(context.WithoutCancel(ctx), volume.ClaimName)
		return SourceVolume{}, err
	}
	return volume, nil
}

func (seeder *SourceSeeder) createClaim(ctx context.Context, name, workflowName string) error {
	// ParseQuantity, not MustParse: this runs on the scheduler's goroutine, and
	// a panic there takes the whole server down over one misconfigured run.
	size, err := resource.ParseQuantity(seeder.config.SourceVolumeSize)
	if err != nil {
		return fmt.Errorf("argojob: source volume size %q is not a Kubernetes quantity: %w",
			seeder.config.SourceVolumeSize, err)
	}
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: seeder.config.Namespace,
			Labels: map[string]string{
				"oberth.ci/component": "source",
				"oberth.ci/workflow":  workflowName,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: size},
			},
		},
	}
	if class := strings.TrimSpace(seeder.config.SourceStorageClass); class != "" {
		claim.Spec.StorageClassName = &class
	}
	_, err = seeder.client.CoreV1().PersistentVolumeClaims(seeder.config.Namespace).
		Create(ctx, claim, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("argojob: create source claim %s: %w", name, err)
	}
	return nil
}

// fill runs a short-lived Pod that mounts the claim and expands the checkout
// into it. The Pod holds no ServiceAccount token and runs read-only apart from
// the claim itself; its whole job is to be a filesystem the exec stream can
// write to.
func (seeder *SourceSeeder) fill(
	ctx context.Context, volume *SourceVolume, sourceDir string, deliverStoreAssets, assistHelper bool, requests ...*Request,
) error {
	podName := volume.ClaimName
	created, err := seeder.createSeedPodBound(ctx, podName, volume.ClaimName)
	if err != nil {
		return err
	}
	stopObserving := seeder.observeCreatedSeed(created, requests...)
	defer func() {
		stopObserving()
		// Best effort: the Pod is disposable, and leaving one behind must not
		// fail a run that otherwise seeded correctly.
		options := *metav1.NewDeleteOptions(0)
		_ = seeder.client.CoreV1().Pods(seeder.config.Namespace).
			Delete(context.WithoutCancel(ctx), podName, options)
	}()

	// A seed left by a previous owner may be deleted during restart cleanup.
	// Retry that stale reuse once; a Pod created by this attempt must still fail
	// if it disappears.
	waitErr := seeder.waitForSeedPod(ctx, podName)
	if created == nil && apierrors.IsNotFound(waitErr) {
		created, err = seeder.createSeedPodBound(ctx, podName, volume.ClaimName)
		if err != nil {
			return err
		}
		stopObserving()
		stopObserving = seeder.observeCreatedSeed(created, requests...)
		waitErr = seeder.waitForSeedPod(ctx, podName)
	}
	if waitErr != nil {
		return waitErr
	}

	reader, writer := io.Pipe()
	go func() {
		writer.CloseWithError(writeSourceArchive(writer, sourceDir))
	}()
	defer func() { _ = reader.Close() }()

	var stdout, stderr strings.Builder
	target := path.Join(seedMountPath, volume.SubPath)
	artifacts := path.Join(seedMountPath, artifactsSubPath)
	command := []string{"/bin/sh", "-c",
		"mkdir -p " + target + " && tar -xzf - -C " + target +
			" && mkdir -p " + artifacts + " && chmod 1777 " + artifacts}
	if err := seeder.exec(ctx, seeder.config.Namespace, podName, seedContainer, command, reader, &stdout, &stderr); err != nil {
		return fmt.Errorf("argojob: stream source checkout into %s: %w: %s",
			volume.ClaimName, err, strings.TrimSpace(stderr.String()))
	}
	volume.ArtifactsSubPath = artifactsSubPath
	if len(requests) == 1 && requests[0] != nil {
		for i, tree := range requests[0].PinnedSources {
			if err := seeder.fillPinnedSource(ctx, podName, i, tree.Directory); err != nil {
				return err
			}
		}
	}
	if err := seeder.fillVaultCA(ctx, volume, podName, deliverStoreAssets); err != nil {
		return err
	}
	if deliverStoreAssets || assistHelper {
		return seeder.fillServerBinary(ctx, volume, podName)
	}
	return nil
}

func (seeder *SourceSeeder) fillPinnedSource(ctx context.Context, podName string, index int, directory string) error {
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() { err := writeSourceArchive(writer, directory); _ = writer.CloseWithError(err); done <- err }()
	target := fmt.Sprintf("/seed/pinned-sources/%d", index)
	command := []string{"/bin/sh", "-c", "mkdir -p " + target + " && tar -xzf - -C " + target}
	// Do not reflect remote output into errors. Only fixed paths enter the shell.
	execErr := seeder.exec(ctx, seeder.config.Namespace, podName, seedContainer, command, reader, io.Discard, io.Discard)
	_ = reader.Close()
	archiveErr := <-done
	if execErr != nil || archiveErr != nil {
		return errors.New("argojob: pinned source delivery failed")
	}
	return nil
}

// fillVaultCA streams the OpenBao TLS trust anchor into the same claim, over
// the same exec, while the seeding Pod is still up.
//
// A credentialed step's first process is secretstore exec, which logs in to OpenBao
// before any Oberth-authored process exists in that container: the anchor has
// to be a file the kubelet can already mount, not content some later process
// writes. The claim is the only filesystem the server can put a file on before
// the Pod starts, and it is already mounted read-only into every container.
//
// The anchor is a certificate -- public material, and server-owned -- so this
// carries nothing the pipeline could not be told; what it carries is which
// certificate the administrator trusts, which is exactly the decision a
// repository must not make for itself.
func (seeder *SourceSeeder) fillVaultCA(
	ctx context.Context, volume *SourceVolume, podName string, deliverStoreAssets bool,
) error {
	pem := seeder.config.VaultCACertPEM
	if !deliverStoreAssets || strings.TrimSpace(pem) == "" {
		// Nothing to deliver: a step without a secret grant holds no token and
		// cannot log in, and a store with a publicly trusted certificate needs
		// no anchor.
		return nil
	}
	target := path.Join(seedMountPath, vaultCASubPath)
	file := path.Join(target, vaultCACertFile)
	// The byte count is checked in the Pod because a short write here is
	// silent: the exec stream closes cleanly either way, and a truncated PEM
	// fails much later as an unparseable trust anchor inside a release step.
	command := []string{"/bin/sh", "-c", fmt.Sprintf(
		"mkdir -p %s && cat > %s && chmod 0444 %s && [ \"$(wc -c < %s)\" -eq %d ]",
		target, file, file, file, len(pem))}

	var stdout, stderr strings.Builder
	if err := seeder.exec(ctx, seeder.config.Namespace, podName, seedContainer, command,
		strings.NewReader(pem), &stdout, &stderr); err != nil {
		return fmt.Errorf("argojob: stream the OpenBao trust anchor into %s: %w: %s",
			volume.ClaimName, err, strings.TrimSpace(stderr.String()))
	}
	volume.VaultCASubPath = vaultCASubPath
	return nil
}

// fillServerBinary streams the running server's own binary into the claim so
// pipeline containers can invoke `oberth secretstore exec` without fetching
// it from a registry. Assisted continuation uses the same verified executable;
// ordinary unassisted steps without secret grants receive no executable.
//
// The binary is verified by SHA-256 readback: the digest is computed during
// the stream and compared against sha256sum run inside the seed pod.
func (seeder *SourceSeeder) fillServerBinary(
	ctx context.Context, volume *SourceVolume, podName string,
) error {
	executablePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("argojob: resolve server executable path: %w", err)
	}
	executable, err := os.Open(executablePath) // #nosec G304 -- os.Executable() returns this process's own path.
	if err != nil {
		return fmt.Errorf("argojob: open server executable: %w", err)
	}
	defer func() { _ = executable.Close() }()

	target := path.Join(seedMountPath, binarySubPath)
	file := path.Join(target, binaryFileName)

	// Stream the binary and compute its digest simultaneously.
	hasher := sha256.New()
	reader := io.TeeReader(executable, hasher)

	command := []string{"/bin/sh", "-c", fmt.Sprintf(
		"mkdir -p %s && cat > %s && chmod 0555 %s",
		target, file, file)}

	var stdout, stderr strings.Builder
	if err := seeder.exec(ctx, seeder.config.Namespace, podName, seedContainer, command,
		reader, &stdout, &stderr); err != nil {
		return fmt.Errorf("argojob: stream server binary into %s: %w: %s",
			volume.ClaimName, err, strings.TrimSpace(stderr.String()))
	}

	expectedDigest := hex.EncodeToString(hasher.Sum(nil))

	// Readback: verify the binary was written correctly.
	verifyCommand := []string{"/bin/sh", "-c", "sha256sum " + file + " | cut -d' ' -f1"}
	var verifyOut, verifyErr strings.Builder
	if err := seeder.exec(ctx, seeder.config.Namespace, podName, seedContainer, verifyCommand,
		nil, &verifyOut, &verifyErr); err != nil {
		return fmt.Errorf("argojob: verify server binary in %s: %w: %s",
			volume.ClaimName, err, strings.TrimSpace(verifyErr.String()))
	}
	actualDigest := strings.TrimSpace(verifyOut.String())
	if actualDigest != expectedDigest {
		return fmt.Errorf("argojob: server binary readback mismatch in %s: expected %s, got %s",
			volume.ClaimName, expectedDigest, actualDigest)
	}

	volume.BinarySubPath = binarySubPath
	return nil
}

func (seeder *SourceSeeder) createSeedPodBound(ctx context.Context, name, claimName string) (*corev1.Pod, error) {
	falseValue := false
	trueValue := true
	rootUser := int64(0)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: seeder.config.Namespace,
			Labels:    map[string]string{"oberth.ci/component": "source-seed"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			// The seeding Pod exists to hold a filesystem open. It must not be
			// able to talk to the API server it was created by.
			AutomountServiceAccountToken: &falseValue,
			ServiceAccountName:           seeder.config.PipelineServiceAccount,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser:      &rootUser,
				RunAsGroup:     &rootUser,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:  seedContainer,
				Image: seeder.config.SourceSeedImage,
				// sleep, not tail -f: the container needs to stay alive for one
				// exec and then be deleted, and an unbounded sleep would keep a
				// leaked Pod running until the namespace is cleaned.
				Command: []string{"/bin/sh", "-c", "sleep 900"},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &falseValue,
					ReadOnlyRootFilesystem:   &trueValue,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "source", MountPath: seedMountPath}},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
						// Checkout bytes live on the PVC. This provisional reserve
						// covers only the seed helper's local logs/overhead.
						corev1.ResourceEphemeralStorage: resource.MustParse("16Mi"),
					},
					// Limits have no prior declaration or exact-step measurement; qualification is pending.
					Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("512Mi"), corev1.ResourceEphemeralStorage: resource.MustParse("64Mi")},
				},
			}},
			Volumes: []corev1.Volume{{
				Name: "source",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claimName},
				},
			}},
		},
	}
	if name == claimName+collectPodSuffix {
		claim, err := seeder.client.CoreV1().PersistentVolumeClaims(seeder.config.Namespace).Get(ctx, claimName, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("argojob: read artifact source claim %s: %w", claimName, err)
		}
		owner, err := sourceWorkflowOwner(claim, strings.TrimSuffix(claimName, "-src"))
		if err != nil {
			return nil, err
		}
		pod.OwnerReferences = []metav1.OwnerReference{owner}
		deadline := int64(artifactCollectionTimeout / time.Second)
		pod.Spec.ActiveDeadlineSeconds = &deadline
	}
	created, err := seeder.client.CoreV1().Pods(seeder.config.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) && name == claimName+collectPodSuffix {
		if err := seeder.removeLeftoverCollector(ctx, pod, claimName); err != nil {
			return nil, err
		}
		// Exactly one new CREATE after observed deletion; never execute the leftover.
		created, err = seeder.client.CoreV1().Pods(seeder.config.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	}
	if apierrors.IsAlreadyExists(err) {
		if name == claimName+collectPodSuffix {
			return nil, fmt.Errorf("argojob: artifact collector Pod already exists: %w", err)
		}
		// A non-nil return is proof that this attempt created the Pod. The typed
		// Kubernetes client may allocate an empty result even on AlreadyExists.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("argojob: create source seed pod %s: %w", name, err)
	}
	return created, nil
}

func (seeder *SourceSeeder) waitForSeedPod(ctx context.Context, name string) error {
	interval := seeder.interval()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		pod, err := seeder.client.CoreV1().Pods(seeder.config.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			switch pod.Status.Phase {
			case corev1.PodRunning:
				return nil
			case corev1.PodFailed, corev1.PodSucceeded:
				return &sourceSeedFailure{failure: fmt.Errorf("argojob: source seed pod %s ended in phase %s before it could be written to", name, pod.Status.Phase), reason: "source seed Pod ended in phase " + string(pod.Status.Phase)}
			case corev1.PodPending, corev1.PodUnknown:
			}
		} else if apierrors.IsNotFound(err) {
			return &sourceSeedFailure{failure: fmt.Errorf("argojob: source seed pod %s disappeared before source preparation completed: %w", name, err), reason: "source seed Pod disappeared"}
		} else {
			return fmt.Errorf("argojob: read source seed pod %s: %w", name, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (seeder *SourceSeeder) interval() time.Duration {
	if seeder.config.SeedPollInterval > 0 {
		return seeder.config.SeedPollInterval
	}
	return time.Second
}

// SweepOrphanedSourceClaims deletes old unowned seeds and source claims that no Workflow owns.
//
// Every claim is normally adopted by its Workflow immediately after submission,
// so the Workflow's own deletion collects it. Two windows escape that: the
// adoption call can fail after the Workflow was created, and the server can
// stop between creating a claim and adopting it. Neither breaks a run, and
// neither is a security exposure -- claim names are unique per run and Admit
// refuses every repository-authored claimName reference -- but an unowned claim
// is never collected by anything, and on long-running infrastructure that fills
// the pipeline namespace one run at a time.
//
// The grace window is what makes this safe to run from Seed: a claim created
// seconds ago may simply be one whose Workflow does not exist yet, including
// this very call's own. Only claims that have had ample time to be adopted and
// still have no owner are collected.
//
// Failures are ignored by design. This is storage hygiene running on the path
// of a real run, and a sweep that cannot list must not stop a build.
func (seeder *SourceSeeder) SweepOrphanedSourceClaims(ctx context.Context) {
	claims := seeder.client.CoreV1().PersistentVolumeClaims(seeder.config.Namespace)
	list, err := claims.List(ctx, metav1.ListOptions{LabelSelector: sourceClaimLabelSelector})
	if err != nil {
		return
	}
	for index := range list.Items {
		claim := &list.Items[index]
		if time.Since(claim.CreationTimestamp.Time) < seeder.orphanGrace() {
			continue
		}
		// A crash before Workflow creation/adoption can leave an ownerless
		// seed. Remove it before the PVC, whose protection finalizer would
		// otherwise wait for that Pod. Also repair this gap on owned claims.
		if !seeder.sweepUnownedSeed(ctx, claim.Name) {
			continue
		}
		if len(claim.OwnerReferences) != 0 {
			continue
		}
		// Before sweeping an unowned claim, check whether the Workflow it
		// was created for still exists and is not terminal. An adoption
		// failure leaves the claim ownerless, but the Workflow may still be
		// running (admitted workflows may run 12 hours). Deleting the claim
		// would destroy a live run's source checkout.
		if workflowName := claim.Labels["oberth.ci/workflow"]; workflowName != "" {
			if seeder.workflowLive(ctx, workflowName) {
				continue
			}
		}
		_ = claims.Delete(ctx, claim.Name, metav1.DeleteOptions{})
	}
}

// workflowLive reports whether the named Workflow exists and has not reached
// a terminal phase. Used by the sweeper to preserve claims whose adoption
// failed but whose Workflow is still running.
func (seeder *SourceSeeder) workflowLive(ctx context.Context, name string) bool {
	// An uncertain or unsynced watch cannot justify collection.
	if seeder.hub == nil || !seeder.hub.Healthy() {
		return true
	}
	workflow, found := seeder.hub.Workflow(name)
	if !found {
		if seeder.workflows == nil {
			return true
		}
		var err error
		workflow, err = seeder.workflows.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false
		}
		if err != nil {
			return true
		}
	}
	return !workflow.Status.Phase.Completed()
}

func (seeder *SourceSeeder) orphanGrace() time.Duration {
	if seeder.config.OrphanClaimGrace > 0 {
		return seeder.config.OrphanClaimGrace
	}
	return defaultOrphanClaimGrace
}

// DeleteClaim removes a run's source claim. It is called when a Workflow is
// cancelled or superseded before it could adopt the claim.
func (seeder *SourceSeeder) DeleteClaim(ctx context.Context, name string) {
	options := metav1.DeleteOptions{}
	_ = seeder.client.CoreV1().PersistentVolumeClaims(seeder.config.Namespace).Delete(ctx, name, options)
}

// AdoptClaim makes the Workflow the owner of the run's claim, so deleting the
// Workflow — by TTL, by supersession, or by hand — collects the storage too.
//
// A surviving source seed is also bound directly to the Workflow.
// This runs after submission because an owner reference needs the owner's UID,
// which only exists once the Workflow does.
func (seeder *SourceSeeder) AdoptClaim(ctx context.Context, name, workflowName, workflowUID string) error {
	claims := seeder.client.CoreV1().PersistentVolumeClaims(seeder.config.Namespace)
	claim, err := claims.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("argojob: read source claim %s: %w", name, err)
	}
	claim.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "argoproj.io/v1alpha1",
		Kind:       "Workflow",
		Name:       workflowName,
		UID:        k8stypes.UID(workflowUID),
	}}
	if _, err := claims.Update(ctx, claim, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("argojob: bind source claim %s to its Workflow: %w", name, err)
	}
	return seeder.adoptSeedPod(ctx, claim, workflowName, workflowUID)
}

// sourceClaimName derives the claim name from the Workflow name, which the
// scheduler already made deterministic and unique per run.
func sourceClaimName(workflowName string) string {
	return workflowName + "-src"
}

// writeSourceArchive streams the checkout as a gzipped tar.
//
// Only regular files and directories are archived. A checkout is the pushed
// revision's content: a symlink escaping the tree, a device node, or a socket
// is not something a pipeline needs, and refusing them here means the extracting
// side cannot be walked outside its own directory.
func writeSourceArchive(destination io.Writer, sourceDir string) error {
	// os.Root confines every open to the checkout, so a symlink that appears
	// mid-walk cannot redirect a read outside it. The walk already refuses to
	// archive symlinks; this closes the window between deciding that and
	// opening the file.
	root, err := os.OpenRoot(sourceDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	compressor := gzip.NewWriter(destination)
	archive := tar.NewWriter(compressor)
	var written int64

	err = fs.WalkDir(root.FS(), ".", func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if current == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return archive.WriteHeader(&tar.Header{
				Name: current + "/", Mode: 0o755, Typeflag: tar.TypeDir, ModTime: info.ModTime(),
			})
		case info.Mode().IsRegular():
			written += info.Size()
			if written > maxSourceArchiveBytes {
				return fmt.Errorf("source checkout exceeds the %d byte limit", int64(maxSourceArchiveBytes))
			}
			if err := archive.WriteHeader(&tar.Header{
				Name: current, Mode: int64(info.Mode().Perm()), Size: info.Size(),
				Typeflag: tar.TypeReg, ModTime: info.ModTime(),
			}); err != nil {
				return err
			}
			file, err := root.Open(current)
			if err != nil {
				return err
			}
			defer func() { _ = file.Close() }()
			_, err = io.Copy(archive, file)
			return err
		default:
			// Symlinks, devices, sockets, FIFOs: skipped deliberately.
			return nil
		}
	})
	if err != nil {
		return err
	}
	if err := archive.Close(); err != nil {
		return err
	}
	return compressor.Close()
}
