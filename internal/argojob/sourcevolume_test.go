package argojob

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// seedTestConfig is a pipeline namespace distinct from the server's, which is
// the whole reason the seeder exists.
func seedTestConfig() Config {
	config := testConfig()
	config.SeedPollInterval = time.Millisecond
	return config
}

// A seeder built from a Config nobody applied defaults to must still work: the
// engine wires one from the same literal it gives the controller, and the
// controller defaults its own copy. Getting this wrong panicked the scheduler
// goroutine on resource.MustParse("") and took the server down with it.
func TestNewSourceSeederAppliesConfigDefaults(t *testing.T) {
	t.Parallel()
	bare := testConfig()
	bare.SourceVolumeSize = ""
	bare.SourceSeedImage = ""
	seeder := NewSourceSeeder(fake.NewClientset(), func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
		return nil
	}, bare)
	if seeder.config.SourceVolumeSize == "" || seeder.config.SourceSeedImage == "" {
		t.Fatalf("seeder kept an undefaulted config: %+v", seeder.config)
	}
}

// A bad size is one failed run, not a dead server.
func TestSeedReportsAnInvalidVolumeSizeInsteadOfPanicking(t *testing.T) {
	t.Parallel()
	config := seedTestConfig()
	config.applyDefaults()
	config.SourceVolumeSize = "not-a-quantity"
	seeder := NewSourceSeeder(fake.NewClientset(), func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
		return nil
	}, config)
	seeder.config.SourceVolumeSize = "not-a-quantity"

	_, err := seeder.Seed(context.Background(), "oberth-oberth-abc-def", writeCheckout(t), false)
	if err == nil || !strings.Contains(err.Error(), "not a Kubernetes quantity") {
		t.Fatalf("expected a reported error, got: %v", err)
	}
}

// runningSeedPods makes the fake clientset report created Pods as Running, the
// way a scheduler would, so the seeder's wait can finish.
func runningSeedPods(client *fake.Clientset) {
	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		pod, ok := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if !ok {
			return false, nil, nil
		}
		pod.Status.Phase = corev1.PodRunning
		return false, pod, nil
	})
}

func writeCheckout(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".oberth"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".oberth", "build.yaml"), []byte("kind: Workflow\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// The seeded claim carries the revision the pipeline is supposed to build.
func TestSeedStreamsTheCheckoutIntoThePipelineNamespace(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset()
	runningSeedPods(client)

	var streamed bytes.Buffer
	var execNamespace, execPod string
	var execCommand []string
	exec := func(_ context.Context, namespace, pod, _ string, command []string,
		stdin io.Reader, _, _ io.Writer,
	) error {
		execNamespace, execPod, execCommand = namespace, pod, command
		_, err := io.Copy(&streamed, stdin)
		return err
	}

	config := seedTestConfig()
	seeder := NewSourceSeeder(client, exec, config)
	if seeder == nil {
		t.Fatal("seeder was not constructed")
	}

	volume, err := seeder.Seed(context.Background(), "oberth-oberth-abc-def", writeCheckout(t), false)
	if err != nil {
		t.Fatal(err)
	}
	if volume.ClaimName != "oberth-oberth-abc-def-src" {
		t.Fatalf("claim name = %q", volume.ClaimName)
	}
	if execNamespace != config.Namespace {
		t.Fatalf("seeded into namespace %q, want the pipeline namespace %q", execNamespace, config.Namespace)
	}
	if execPod != volume.ClaimName {
		t.Fatalf("seed pod %q does not match its claim %q", execPod, volume.ClaimName)
	}
	if !strings.Contains(strings.Join(execCommand, " "), "tar -xzf -") {
		t.Fatalf("seed command does not extract an archive: %v", execCommand)
	}

	names := archiveNames(t, streamed.Bytes())
	for _, want := range []string{".oberth/build.yaml", "go.mod"} {
		if !names[want] {
			t.Errorf("checkout archive is missing %s (got %v)", want, keys(names))
		}
	}

	claim, err := client.CoreV1().PersistentVolumeClaims(config.Namespace).
		Get(context.Background(), volume.ClaimName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(claim.Spec.AccessModes) != 1 || claim.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Fatalf("claim access modes = %v", claim.Spec.AccessModes)
	}
}

// A run whose source could not be delivered must not leave storage behind in
// the pipeline namespace.
func TestSeedRemovesTheClaimWhenStreamingFails(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset()
	runningSeedPods(client)

	exec := func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
		return errors.New("connection reset")
	}
	config := seedTestConfig()
	seeder := NewSourceSeeder(client, exec, config)

	if _, err := seeder.Seed(context.Background(), "oberth-oberth-abc-def", writeCheckout(t), false); err == nil {
		t.Fatal("expected the streaming failure to surface")
	}
	_, err := client.CoreV1().PersistentVolumeClaims(config.Namespace).
		Get(context.Background(), "oberth-oberth-abc-def-src", metav1.GetOptions{})
	if err == nil {
		t.Fatal("the claim outlived the failed seed")
	}
}

// The seeding Pod exists to hold a filesystem open. It must not be able to talk
// to the API server that created it.
func TestSeedPodHoldsNoServiceAccountToken(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset()
	runningSeedPods(client)
	var created *corev1.Pod
	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		if pod, ok := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod); ok {
			created = pod
		}
		return false, nil, nil
	})

	seeder := NewSourceSeeder(client, func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
		return nil
	}, seedTestConfig())
	if _, err := seeder.Seed(context.Background(), "oberth-oberth-abc-def", writeCheckout(t), false); err != nil {
		t.Fatal(err)
	}
	if created == nil {
		t.Fatal("no seed pod was created")
	}
	if created.Spec.AutomountServiceAccountToken == nil || *created.Spec.AutomountServiceAccountToken {
		t.Fatal("the seed pod mounts a ServiceAccount token")
	}
	container := created.Spec.Containers[0]
	if container.SecurityContext == nil || container.SecurityContext.ReadOnlyRootFilesystem == nil ||
		!*container.SecurityContext.ReadOnlyRootFilesystem {
		t.Fatal("the seed pod's root filesystem is writable")
	}
}

// A pipeline cannot be submitted without a source volume: the alternative is a
// Workflow whose first step runs in an empty directory.
func TestCreateRefusesWithoutASeeder(t *testing.T) {
	t.Parallel()
	controller, err := NewController(unusedWorkflows{}, nil, seedTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Create(context.Background(), Request{RunID: "r", Name: "n"}); err == nil {
		t.Fatal("expected a refusal when no seeder is configured")
	}
}

// Symlinks, devices and sockets are not part of a pushed revision, and refusing
// them means the extracting side cannot be walked out of its own directory.
func TestArchiveSkipsNonRegularEntries(t *testing.T) {
	t.Parallel()
	root := writeCheckout(t)
	if err := os.Symlink("/etc/passwd", filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	var buffer bytes.Buffer
	if err := writeSourceArchive(&buffer, root); err != nil {
		t.Fatal(err)
	}
	if archiveNames(t, buffer.Bytes())["escape"] {
		t.Fatal("the archive carried a symlink out of the checkout")
	}
}

func archiveNames(t *testing.T, archive []byte) map[string]bool {
	t.Helper()
	decompressor, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	reader := tar.NewReader(decompressor)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names[header.Name] = true
	}
}

func keys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	return out
}

// unusedWorkflows proves the refusal happens before any submission is attempted.
type unusedWorkflows struct{}

func (unusedWorkflows) Create(context.Context, *wfv1.Workflow, metav1.CreateOptions) (*wfv1.Workflow, error) {
	panic("Create must not reach the Workflow client without a source volume")
}

func (unusedWorkflows) Get(context.Context, string, metav1.GetOptions) (*wfv1.Workflow, error) {
	panic("Get must not reach the Workflow client without a source volume")
}

func (unusedWorkflows) Delete(context.Context, string, metav1.DeleteOptions) error {
	panic("Delete must not reach the Workflow client without a source volume")
}

// An unowned claim is never collected by anything: the Workflow that would have
// owned it does not exist, so Kubernetes garbage collection has nothing to act
// on. On long-running infrastructure that fills the pipeline namespace one run
// at a time.
func TestSweepCollectsOrphanedSourceClaims(t *testing.T) {
	t.Parallel()
	config := seedTestConfig()
	config.applyDefaults()
	config.OrphanClaimGrace = time.Minute

	orphan := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "abandoned-src", Namespace: config.Namespace,
			Labels:            map[string]string{"oberth.ci/component": "source"},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-2 * time.Hour)),
		},
	}
	owned := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "owned-src", Namespace: config.Namespace,
			Labels:            map[string]string{"oberth.ci/component": "source"},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-2 * time.Hour)),
			OwnerReferences:   []metav1.OwnerReference{{Kind: "Workflow", Name: "live", UID: "u"}},
		},
	}
	// Young enough that its Workflow may simply not exist yet -- including a
	// claim this very sweep's own run is about to adopt.
	fresh := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "fresh-src", Namespace: config.Namespace,
			Labels:            map[string]string{"oberth.ci/component": "source"},
			CreationTimestamp: metav1.NewTime(time.Now()),
		},
	}
	// Not ours: no component label. Must be left alone entirely.
	foreign := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "someone-elses", Namespace: config.Namespace,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-2 * time.Hour)),
		},
	}
	client := fake.NewClientset(orphan, owned, fresh, foreign)
	seeder := NewSourceSeeder(client, func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
		return nil
	}, config)

	seeder.SweepOrphanedSourceClaims(context.Background())

	remaining := map[string]bool{}
	list, err := client.CoreV1().PersistentVolumeClaims(config.Namespace).
		List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, claim := range list.Items {
		remaining[claim.Name] = true
	}
	if remaining["abandoned-src"] {
		t.Error("the orphaned claim was not collected")
	}
	for _, name := range []string{"owned-src", "fresh-src", "someone-elses"} {
		if !remaining[name] {
			t.Errorf("the sweep deleted %s, which it must not touch", name)
		}
	}
}

// testVaultCAPEM is a real, self-signed certificate: the seeder and Validate
// both parse what they are given, so a placeholder string would prove nothing
// about either.
func testVaultCAPEM(t *testing.T) string {
	t.Helper()
	return generateTestVaultCAPEM()
}

// generateTestVaultCAPEM is the same certificate without a *testing.T, so the
// package-level fixture configuration can hold one too.
func generateTestVaultCAPEM() string {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "openbao-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// recordingExec captures every exec the seeder performs, with the bytes it
// streamed, so a test can assert on the second one without a cluster.
type seedExecCall struct {
	command []string
	stdin   string
}

func recordingExec(calls *[]seedExecCall) ExecStreamer {
	// lastStreamedDigest tracks the SHA-256 of the most recently streamed
	// non-empty stdin, so the mock can answer sha256sum readback checks that
	// fillServerBinary performs.
	var lastStreamedDigest string
	return func(_ context.Context, _, _, _ string, command []string,
		stdin io.Reader, stdout, _ io.Writer,
	) error {
		var streamed bytes.Buffer
		if stdin != nil {
			if _, err := io.Copy(&streamed, stdin); err != nil {
				return err
			}
		}
		*calls = append(*calls, seedExecCall{command: command, stdin: streamed.String()})
		if streamed.Len() > 0 {
			digest := sha256.Sum256(streamed.Bytes())
			lastStreamedDigest = hex.EncodeToString(digest[:])
		}
		// When the command is a sha256sum readback, write the last streamed
		// content's digest to stdout so the verification succeeds.
		joined := strings.Join(command, " ")
		if strings.Contains(joined, "sha256sum") && lastStreamedDigest != "" {
			_, _ = fmt.Fprintln(stdout, lastStreamedDigest)
		}
		return nil
	}
}

// A credentialed run's claim carries the trust anchor beside the checkout. This
// is the delivery mechanism itself: without it a credentialed step reaches an
// HTTPS OpenBao with nothing to verify it against, and the login fails before
// any credential exists.
func TestSeedDeliversTheVaultTrustAnchorOnTheReleaseTier(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset()
	runningSeedPods(client)

	var calls []seedExecCall
	config := seedTestConfig()
	config.VaultCACertPEM = testVaultCAPEM(t)
	seeder := NewSourceSeeder(client, recordingExec(&calls), config)

	volume, err := seeder.Seed(context.Background(), "oberth-oberth-abc-def", writeCheckout(t), true)
	if err != nil {
		t.Fatal(err)
	}
	if volume.VaultCASubPath != vaultCASubPath {
		t.Fatalf("VaultCASubPath = %q, want %q", volume.VaultCASubPath, vaultCASubPath)
	}
	// Exec calls: checkout, anchor, binary stream, binary sha256sum readback.
	if len(calls) != 4 {
		t.Fatalf("expected 4 execs (checkout, anchor, binary, verify), got %d", len(calls))
	}
	anchor := calls[1]
	if anchor.stdin != config.VaultCACertPEM {
		t.Fatalf("streamed anchor does not match the configured PEM:\n%q", anchor.stdin)
	}
	joined := strings.Join(anchor.command, " ")
	// The anchor lands beside the checkout, never inside it: a repository must
	// not be able to shadow the certificate its own release verifies with.
	if !strings.Contains(joined, "/seed/"+vaultCASubPath+"/"+vaultCACertFile) {
		t.Fatalf("anchor written outside its own subPath: %v", anchor.command)
	}
	if strings.Contains(joined, "/seed/"+volume.SubPath+"/") {
		t.Fatalf("anchor written into the checkout: %v", anchor.command)
	}
	// A short write closes the exec stream cleanly, so the Pod is what has to
	// notice. Without this check a truncated PEM surfaces much later, as an
	// unparseable anchor inside a release step.
	if !strings.Contains(joined, "wc -c") {
		t.Fatalf("anchor delivery does not verify the byte count: %v", anchor.command)
	}
}

// A non-credentialed CI run gets no anchor at all. Its step containers hold no
// ServiceAccount token and cannot log in, so an anchor there would be an unused
// file that makes the tier boundary look like a matter of configuration.
func TestSeedWithholdsTheVaultTrustAnchorFromTheCITier(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset()
	runningSeedPods(client)

	var calls []seedExecCall
	config := seedTestConfig()
	config.VaultCACertPEM = testVaultCAPEM(t)
	seeder := NewSourceSeeder(client, recordingExec(&calls), config)

	volume, err := seeder.Seed(context.Background(), "oberth-oberth-abc-def", writeCheckout(t), false)
	if err != nil {
		t.Fatal(err)
	}
	if volume.VaultCASubPath != "" {
		t.Fatalf("a CI run was given a trust anchor at %q", volume.VaultCASubPath)
	}
	if len(calls) != 1 {
		t.Fatalf("expected only the checkout to be streamed, got %d execs", len(calls))
	}
}

// A failed anchor delivery fails the run. Continuing would submit a Workflow
// whose release steps cannot authenticate, and the failure would be reported
// against the step rather than against the delivery that never happened.
func TestSeedFailsWhenTheTrustAnchorCannotBeDelivered(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset()
	runningSeedPods(client)

	var attempts int
	exec := func(_ context.Context, _, _, _ string, _ []string, stdin io.Reader, _, _ io.Writer) error {
		attempts++
		if stdin != nil {
			_, _ = io.Copy(io.Discard, stdin)
		}
		if attempts == 2 {
			return errors.New("command terminated with exit code 1")
		}
		return nil
	}
	config := seedTestConfig()
	config.VaultCACertPEM = testVaultCAPEM(t)
	seeder := NewSourceSeeder(client, exec, config)

	if _, err := seeder.Seed(context.Background(), "oberth-oberth-abc-def", writeCheckout(t), true); err == nil ||
		!strings.Contains(err.Error(), "trust anchor") {
		t.Fatalf("expected a reported anchor failure, got: %v", err)
	}
	// The claim goes with it: a failed submission must not leave storage in the
	// pipeline namespace.
	claims, err := client.CoreV1().PersistentVolumeClaims(config.Namespace).
		List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(claims.Items) != 0 {
		t.Fatalf("a failed seeding left %d claim(s) behind", len(claims.Items))
	}
}
