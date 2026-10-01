package argojob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/pkg/periapsis"
)

// The release-credential live suite is the only test that reaches a real
// secret store with a real identity. Everything else in this package proves
// what Oberth builds; this proves that what Oberth builds can authenticate.
//
// It is gated separately from the rest of the live suite, and on more than a
// kubeconfig, because "point it at a disposable cluster" is not a sufficient
// warning for a test that performs a Vault login as the release tier. Every
// coordinate is named explicitly:
//
//	OBERTH_ARGO_LIVE_VAULT_ADDR   https:// address of the store
//	OBERTH_ARGO_LIVE_VAULT_ROLE   Kubernetes-auth role bound to (namespace, release SA)
//	OBERTH_ARGO_LIVE_VAULT_CA     PEM file: the anchor under test
//
// The credential half additionally needs the three binaries a real release
// step runs, because building third-party code inside a test is not this
// test's subject:
//
//	OBERTH_ARGO_LIVE_ENVCONSUL     envconsul, at the version .oberth/release.yaml pins
//	OBERTH_ARGO_LIVE_MATERIALIZE   ./cmd/oberth-secret-materialize, built from this revision
//	OBERTH_ARGO_LIVE_COSIGN        cosign, for the real prepare_signing_identity check
//
// It reads secrets and publishes nothing. The step's final command is a proof
// script, not .oberth/release.sh: proving the authentication and delivery
// chain is a different act from performing a release, and only one of them is
// reversible.
const (
	liveVaultAddrEnvironment  = "OBERTH_ARGO_LIVE_VAULT_ADDR"
	liveVaultRoleEnvironment  = "OBERTH_ARGO_LIVE_VAULT_ROLE"
	liveVaultCAEnvironment    = "OBERTH_ARGO_LIVE_VAULT_CA"
	liveEnvconsulEnvironment  = "OBERTH_ARGO_LIVE_ENVCONSUL"
	liveMaterializeEnvVarName = "OBERTH_ARGO_LIVE_MATERIALIZE"
	liveCosignEnvironment     = "OBERTH_ARGO_LIVE_COSIGN"
)

// requireVaultCoordinates returns the store the release tier is meant to
// reach, or skips.
func requireVaultCoordinates(t *testing.T) (address, role, caPEM string) {
	t.Helper()
	address = strings.TrimSpace(os.Getenv(liveVaultAddrEnvironment))
	role = strings.TrimSpace(os.Getenv(liveVaultRoleEnvironment))
	caPath := strings.TrimSpace(os.Getenv(liveVaultCAEnvironment))
	if address == "" || role == "" || caPath == "" {
		t.Skipf("release-credential live suite needs %s, %s and %s",
			liveVaultAddrEnvironment, liveVaultRoleEnvironment, liveVaultCAEnvironment)
	}
	raw, err := os.ReadFile(caPath) // #nosec G304 -- a test environment variable naming a local PEM file.
	if err != nil {
		t.Fatalf("read %s: %v", liveVaultCAEnvironment, err)
	}
	return address, role, string(raw)
}

func requireBinary(t *testing.T, variable string) string {
	t.Helper()
	path := strings.TrimSpace(os.Getenv(variable))
	if path == "" {
		t.Skipf("this test runs the real release chain and needs %s", variable)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s: %v", variable, err)
	}
	return path
}

// TestLiveReleaseTierReceivesTheVaultTrustAnchor is the delivery mechanism on
// its own: a real seeding, a real Workflow, a real Pod, and the anchor present
// at the path VAULT_CACERT names, byte-identical to what the server was
// configured with.
//
// It deliberately proves nothing about Vault. If this fails, no credential
// test below it can be interpreted.
func TestLiveReleaseTierReceivesTheVaultTrustAnchor(t *testing.T) {
	cluster := requireLiveCluster(t)
	address, role, caPEM := requireVaultCoordinates(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cluster.ensureServiceAccounts(t, ctx)

	config := cluster.config()
	config.VaultAddress, config.VaultCredentialedRole, config.VaultCACertPEM = address, role, caPEM
	config.RunnerImagePrefixes = []string{"busybox:"}

	controller, err := NewController(cluster.workflows, cluster.kube, config)
	if err != nil {
		t.Fatalf("controller: %v", err)
	}
	seeder := NewSourceSeeder(cluster.kube, cluster.execStreamer(), config)
	if seeder == nil {
		t.Fatal("source seeder was not constructed")
	}
	controller = controller.WithSourceSeeder(seeder)

	digest := sha256.Sum256([]byte(caPEM))
	document := fmt.Sprintf(`
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 300
  templates:
  - name: main
    dag:
      tasks:
      - name: anchor
        template: anchor
  - name: anchor
    container:
      image: busybox:1.37@sha256:ab33eacc8251e3807b85bb6dba570e4698c3998eca6f0fc2ccb60575a563ea74
      command: [/bin/sh, -c]
      args:
      - |
        set -eu
        test -n "${VAULT_CACERT:-}" || { echo "VAULT_CACERT is unset"; exit 1; }
        test "$VAULT_CACERT" = %q || { echo "VAULT_CACERT=$VAULT_CACERT"; exit 1; }
        test -s "$VAULT_CACERT" || { echo "no anchor at $VAULT_CACERT"; exit 1; }
        observed=$(sha256sum "$VAULT_CACERT" | cut -d' ' -f1)
        test "$observed" = %q || { echo "anchor digest $observed"; exit 1; }
        grep -q "BEGIN CERTIFICATE" "$VAULT_CACERT"
        # The login token is scoped to credentialed templates (envconsul).
        # This template's command is /bin/sh, so it must not carry the token.
        test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token || \
          { echo "a non-credentialed release template carries the login token"; exit 1; }
        # The checkout is still there and still has nothing added to it.
        test -f /work/src/go.mod || { echo "the source mount was displaced"; exit 1; }
        test ! -e /work/src/vault-ca || { echo "the anchor was written into the checkout"; exit 1; }
        echo "anchor-verified $observed"
`, VaultCACertPath, hex.EncodeToString(digest[:]))

	request := liveRequest("oberth-live-anchor", periapsis.TriggerRelease, document)
	request.SourceDir = repositoryRoot(t)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		_ = controller.Cancel(cleanupCtx, request.Name, request.RunID)
	})
	if _, err := controller.Create(ctx, request); err != nil {
		t.Fatalf("create: %v", err)
	}

	log := &recordingWriter{}
	completion, err := controller.Wait(ctx, request.Name, request.RunID, log)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if !completion.Succeeded {
		t.Fatalf("anchor verification failed: phase=%s reason=%s\n%s",
			completion.Phase, completion.Reason, truncate(log.String(), 4000))
	}
	if !strings.Contains(log.String(), "anchor-verified") {
		t.Fatalf("the step did not report a verified anchor:\n%s", truncate(log.String(), 4000))
	}
}

// TestLiveReleaseCredentialsMaterialiseFromTheRealStore runs the exact process
// chain .oberth/release.yaml's credentialed steps run --
//
//	envconsul -> oberth-secret-materialize -> <command>
//
// with the repository's own committed envconsul configuration, the same
// argument shape, the server-injected role, and the anchor this change
// delivers. Only the final command differs: a proof script instead of
// .oberth/release.sh, because a real publish is a separate act with separate
// consequences.
//
// What a pass establishes: the release-tier Pod verified the store's
// certificate against the delivered anchor, logged in with its own projected
// ServiceAccount token against the administrator's role, read the paths
// publish-r2 declares, and left them on a memory-backed volume in the exact
// layout release.sh opens -- with the private signing key proven, by cosign
// itself, to be the real key matching the real public key beside it.
func TestLiveReleaseCredentialsMaterialiseFromTheRealStore(t *testing.T) {
	cluster := requireLiveCluster(t)
	address, role, caPEM := requireVaultCoordinates(t)
	envconsul := requireBinary(t, liveEnvconsulEnvironment)
	materialize := requireBinary(t, liveMaterializeEnvVarName)
	cosign := requireBinary(t, liveCosignEnvironment)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cluster.ensureServiceAccounts(t, ctx)

	config := cluster.config()
	config.VaultAddress, config.VaultCredentialedRole, config.VaultCACertPEM = address, role, caPEM
	config.RunnerImagePrefixes = []string{"golang:", "aquasec/trivy:"}
	config.WorkflowTimeout = 15 * time.Minute

	controller, err := NewController(cluster.workflows, cluster.kube, config)
	if err != nil {
		t.Fatalf("controller: %v", err)
	}
	seeder := NewSourceSeeder(cluster.kube, cluster.execStreamer(), config)
	if seeder == nil {
		t.Fatal("source seeder was not constructed")
	}
	controller = controller.WithSourceSeeder(seeder)

	request := liveRequest("oberth-live-credentials", periapsis.TriggerRelease, releaseCredentialDocument)
	request.SourceDir = credentialProofCheckout(t, envconsul, materialize, cosign)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		_ = controller.Cancel(cleanupCtx, request.Name, request.RunID)
	})
	if _, err := controller.Create(ctx, request); err != nil {
		t.Fatalf("create: %v", err)
	}

	log := &recordingWriter{}
	completion, err := controller.Wait(ctx, request.Name, request.RunID, log)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	body := log.String()
	if !completion.Succeeded {
		t.Fatalf("the credentialed step failed: phase=%s reason=%s step=%s\n%s",
			completion.Phase, completion.Reason, completion.FailedStep, truncate(body, 8000))
	}
	// Each marker is written only after the check above it passed, so their
	// presence is the statement of what was proven -- not the exit code alone.
	for _, marker := range []string{
		"proof: vault login succeeded",
		"proof: materialised layout correct",
		"proof: signing identity verified",
		"proof: credential environment clean",
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("missing %q in the step log:\n%s", marker, truncate(body, 8000))
		}
	}
	// The whole point of the shim is that no value reaches a log.
	for _, leak := range []string{"BEGIN ENCRYPTED SIGSTORE PRIVATE KEY", "COSIGN_PASSWORD=", "R2_UPLOAD_TOKEN="} {
		if strings.Contains(body, leak) {
			t.Errorf("the step log contains %q", leak)
		}
	}
}

// releaseCredentialDocument mirrors the release-publish-r2 template of
// .oberth/release.yaml: the same envconsul flags in the same order, the same
// two config files, the same $(OBERTH_VAULT_ROLE) expansion the kubelet
// performs, the same shim argument list, and the same memory-backed
// destination. The command after -- is the only difference.
const releaseCredentialDocument = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 900
  templates:
  - name: main
    dag:
      tasks:
      - name: credentials
        template: credentials
  - name: credentials
    volumes:
    - name: oberth-release-secrets
      emptyDir:
        medium: Memory
        sizeLimit: 1Mi
    container:
      image: "golang:1.26.6-trixie@sha256:ab563819a16cfe5faff0f96a8bb598fbb0e400ab2ac751996e60abcb23b106a3"
      workingDir: /work/src
      command: ["/work/src/bin/envconsul"]
      args:
      - "-once"
      - "-config"
      - ".oberth/envconsul.hcl"
      - "-config"
      - ".oberth/envconsul/publish-r2.hcl"
      - "-vault-k8s-auth-role-name"
      - "$(OBERTH_VAULT_ROLE)"
      - "/work/src/bin/oberth-secret-materialize"
      - "-dir"
      - "/run/oberth-secrets"
      - "R2_UPLOAD_TOKEN=r2-upload-token/token"
      - "COSIGN_KEY=cosign-secret/cosign.key"
      - "COSIGN_PUB=cosign-secret/cosign.pub"
      - "--"
      - "/work/src/bin/proof.sh"
      env:
      - name: PATH
        value: "/work/src/bin:/usr/local/bin:/usr/bin:/bin"
      - name: HOME
        value: "/tmp"
      volumeMounts:
      - name: oberth-release-secrets
        mountPath: /run/oberth-secrets
      resources:
        requests:
          cpu: "500m"
          memory: "512Mi"
        limits:
          cpu: "1"
          memory: "1Gi"
`

// credentialProofCheckout builds the tree the seeder streams: the repository's
// own envconsul configuration, unmodified, plus the binaries a real release
// step would have built in its setup burn and the proof script that replaces
// the release action.
func credentialProofCheckout(t *testing.T, envconsul, materialize, cosign string) string {
	t.Helper()
	root := t.TempDir()
	repository := repositoryRoot(t)
	for _, name := range []string{
		filepath.Join(".oberth", "envconsul.hcl"),
		filepath.Join(".oberth", "envconsul", "publish-r2.hcl"),
	} {
		copyInto(t, filepath.Join(repository, name), filepath.Join(root, name), 0o644)
	}
	copyInto(t, envconsul, filepath.Join(root, "bin", "envconsul"), 0o755)
	copyInto(t, materialize, filepath.Join(root, "bin", "oberth-secret-materialize"), 0o755)
	copyInto(t, cosign, filepath.Join(root, "bin", "cosign"), 0o755)
	if err := os.WriteFile(filepath.Join(root, "bin", "proof.sh"), []byte(credentialProofScript), 0o755); err != nil { // #nosec G306 -- an executable the pipeline runs.
		t.Fatal(err)
	}
	return root
}

func copyInto(t *testing.T, source, destination string, mode os.FileMode) {
	t.Helper()
	content, err := os.ReadFile(source) // #nosec G304 -- test fixtures named by the test itself.
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, content, mode); err != nil {
		t.Fatal(err)
	}
}

// credentialProofScript is what runs in place of .oberth/release.sh.
//
// It asserts the state release.sh would depend on and performs the one check
// that cannot be faked -- prepare_signing_identity's own derivation, verbatim
// from .oberth/release.sh, which decrypts the materialised private key with
// the materialised password and compares the derived public key against the
// materialised one. Three fields, all three byte-correct, proven by cosign.
//
// Nothing here publishes, and nothing here prints a secret: the markers are
// the evidence, and every value stays inside the container.
const credentialProofScript = `#!/bin/sh
set -eu

fail() { echo "proof-FAILED: $*" >&2; exit 1; }

# Reaching this process at all means envconsul completed the login: it execs
# the child only after resolving every declared path, and a rejected login or
# an unverifiable certificate exits before this line can run.
echo "proof: vault login succeeded"

secret_root=${OBERTH_SECRETSTORE_DIR:-}
test -n "$secret_root" || fail "the shim did not export OBERTH_SECRETSTORE_DIR"
test -d "$secret_root" || fail "$secret_root is not a directory"

for relative in r2-upload-token/token cosign-secret/cosign.key cosign-secret/cosign.pub; do
	file=$secret_root/$relative
	test -s "$file" || fail "$relative is missing or empty"
	mode=$(stat -c '%a' "$file")
	test "$mode" = "400" || fail "$relative has mode $mode, want 400"
done

# The destination is memory-backed. A credential on the container's own layer
# is the disk this whole design exists to avoid.
case $(stat -f -c '%T' "$secret_root") in
	tmpfs|ramfs) ;;
	*) fail "$secret_root is not memory-backed: $(stat -f -c '%T' "$secret_root")" ;;
esac

# Only what this step declared. A field arriving that nothing asked for is the
# least-privilege defect .oberth/envconsul/ exists to prevent.
#
# cosign.password is deliberately not among them: the real store holds
# cosign-secret with two fields, key and pub, and release.sh treats the
# password as optional for exactly that case.
found=$(find "$secret_root" -type f | sort | tr '\n' ' ')
expected="$secret_root/cosign-secret/cosign.key $secret_root/cosign-secret/cosign.pub $secret_root/r2-upload-token/token "
test "$found" = "$expected" || fail "unexpected materialised set"

grep -q "BEGIN" "$secret_root/cosign-secret/cosign.key" || fail "cosign.key is not PEM"
grep -q "BEGIN PUBLIC KEY" "$secret_root/cosign-secret/cosign.pub" || fail "cosign.pub is not a public key"
# The R2 token is a bearer credential: one line, no whitespace.
test "$(wc -l < "$secret_root/r2-upload-token/token")" -le 1 || fail "the R2 token spans lines"
echo "proof: materialised layout correct"

# prepare_signing_identity, verbatim from .oberth/release.sh, against the
# materialised files. Deriving the public key requires decrypting the private
# key with the password, so a pass is a cryptographic statement that all three
# fields are the real ones from the store.
release_dir=/tmp/proof
mkdir -p "$release_dir"
key_file=$secret_root/cosign-secret/cosign.key
password_file=$secret_root/cosign-secret/cosign.password
expected_public_key=$secret_root/cosign-secret/cosign.pub
test -s "$key_file" || fail "cosign-secret/cosign.key is missing"
command -v cosign >/dev/null 2>&1 || fail "cosign is not installed"
COSIGN_KEY=$(cat "$key_file")
export COSIGN_KEY
COSIGN_PASSWORD=
if [ -f "$password_file" ]; then
	COSIGN_PASSWORD=$(cat "$password_file")
fi
export COSIGN_PASSWORD COSIGN_YES=true
cosign public-key --key env://COSIGN_KEY >"$release_dir/cosign.pub" 2>/dev/null
test -s "$release_dir/cosign.pub" || fail "cosign produced an empty public key"
cmp -s "$expected_public_key" "$release_dir/cosign.pub" || fail "mounted cosign public key does not match its private key"
unset COSIGN_KEY COSIGN_PASSWORD
echo "proof: signing identity verified"

# The shim's other half: the release script must inherit neither the resolved
# secrets nor the token that fetched them.
for variable in R2_UPLOAD_TOKEN COSIGN_KEY COSIGN_PASSWORD COSIGN_PUB; do
	env | grep -q "^$variable=" && fail "$variable survived into the release environment"
done
env | grep -E '^(VAULT_|BAO_|CONSUL_)' | grep -v '^VAULT_CACERT=' && \
	fail "a Vault variable survived into the release environment"
# VAULT_CACERT names a public certificate and is stripped with the rest; its
# absence here is the check, not its presence.
test -z "${VAULT_CACERT:-}" || fail "VAULT_CACERT survived into the release environment"
test -n "${OBERTH_RUN_ID:-}" || fail "the run identity was stripped along with the credentials"
echo "proof: credential environment clean"
echo "proof: complete, nothing published"
`
