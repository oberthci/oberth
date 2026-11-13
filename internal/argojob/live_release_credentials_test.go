package argojob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/pkg/pipeline"
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
const (
	liveVaultAddrEnvironment = "OBERTH_ARGO_LIVE_VAULT_ADDR"
	liveVaultRoleEnvironment = "OBERTH_ARGO_LIVE_VAULT_ROLE"
	liveVaultCAEnvironment   = "OBERTH_ARGO_LIVE_VAULT_CA"
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
        # The login token is scoped to credentialed templates (secretstore exec).
        # This template's command is /bin/sh, so it must not carry the token.
        test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token || \
          { echo "a non-credentialed release template carries the login token"; exit 1; }
        # The checkout is still there and still has nothing added to it.
        test -f /work/src/go.mod || { echo "the source mount was displaced"; exit 1; }
        test ! -e /work/src/vault-ca || { echo "the anchor was written into the checkout"; exit 1; }
        echo "anchor-verified $observed"
`, VaultCACertPath, hex.EncodeToString(digest[:]))

	request := liveRequest("oberth-live-anchor", pipeline.TriggerRelease, document)
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
	completion, err := withWaitTestHub(t, controller, cluster.hub).Wait(ctx, request.Name, request.RunID, log)
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
